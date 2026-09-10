// Package storagebarrier coordinates instance storage operations with migration.
package storagebarrier

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/redis/go-redis/v9"
)

var (
	ErrBusy  = errors.New("storage migration in progress; retry later")
	ErrStale = errors.New("storage backend changed; reload the instance and retry")
	ErrLost  = errors.New("storage barrier state lost; manual recovery required")
)

// ponytail: permits deliberately do not expire. A crashed writer requires
// operator recovery; automatic recovery needs storage-level fencing first.
const script = `
local action, token, generation = ARGV[1], ARGV[2], ARGV[3]
if action == 'enter' or action == 'close' then
  redis.call('hsetnx', KEYS[1], 'generation', generation)
  if redis.call('hget', KEYS[1], 'generation') ~= generation then return -2 end
  if redis.call('hexists', KEYS[1], 'owner') == 1 then return -1 end
  if action == 'enter' then redis.call('hset', KEYS[1], token, '1')
  else redis.call('hset', KEYS[1], 'owner', token) end
  return 0
end
if action == 'leave' then
  if redis.call('hdel', KEYS[1], token) ~= 1 then return -3 end
  return 0
end
if redis.call('hget', KEYS[1], 'owner') ~= token then return -3 end
if action == 'check' then return redis.call('hlen', KEYS[1]) - 2 end
if action == 'open' then
  local current = redis.call('hget', KEYS[1], 'generation')
  if #generation < #current or (#generation == #current and generation < current) then return -2 end
  if redis.call('hlen', KEYS[1]) ~= 2 then return -1 end
  redis.call('hset', KEYS[1], 'generation', generation)
  redis.call('hdel', KEYS[1], 'owner')
  return 0
end
if action == 'cancel' then
  redis.call('hdel', KEYS[1], 'owner')
  return 0
end
return -3`

type state struct {
	generation int64
	owner      string
	readers    map[string]bool
}

// Service uses the configured lock Redis, or memory in a single-process stack.
type Service struct {
	client redis.UniversalClient
	mu     sync.Mutex
	states map[string]*state
}

func New(client redis.UniversalClient) *Service {
	return &Service{client: client, states: make(map[string]*state)}
}

func (s *Service) operation(ctx context.Context, key, action, token string, generation int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.client != nil {
		n, err := s.client.Eval(ctx, script, []string{"storage-barrier:" + key}, action, token, strconv.FormatInt(generation, 10)).Int64()
		if err != nil {
			return 0, fmt.Errorf("storage barrier: %w", err)
		}
		switch n {
		case -1:
			return 0, ErrBusy
		case -2:
			return 0, ErrStale
		case -3:
			return 0, ErrLost
		}
		return n, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[key]
	if st == nil {
		if action != "enter" && action != "close" {
			return 0, ErrLost
		}
		st = &state{generation: generation, readers: make(map[string]bool)}
		s.states[key] = st
	}
	switch action {
	case "enter", "close":
		if st.generation != generation {
			return 0, ErrStale
		}
		if st.owner != "" {
			return 0, ErrBusy
		}
		if action == "enter" {
			st.readers[token] = true
		} else {
			st.owner = token
		}
	case "leave":
		if !st.readers[token] {
			return 0, ErrLost
		}
		delete(st.readers, token)
	case "check", "open", "cancel":
		if st.owner != token {
			return 0, ErrLost
		}
		if action == "check" {
			return int64(len(st.readers)), nil
		}
		if action == "cancel" {
			st.owner = ""
			return 0, nil
		}
		if generation < st.generation {
			return 0, ErrStale
		}
		if len(st.readers) != 0 {
			return 0, ErrBusy
		}
		st.generation, st.owner = generation, ""
	default:
		return 0, ErrLost
	}
	return 0, nil
}

type operation struct {
	service    *Service
	key, token string
	generation int64
	mu         sync.Mutex
	refs       int
}

// Permit covers an operation through its last storage/index write. Forked
// permits allow an already admitted request/job to finish during draining.
type Permit struct {
	op     *operation
	once   sync.Once
	err    error
	closed bool // guarded by op.mu
}

// Enter admits an operation for a freshly loaded storage generation. A live
// parent lets its nested operations finish after new admission is closed.
func (s *Service) Enter(ctx context.Context, key string, generation int64, parent *Permit) (*Permit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" || generation < 0 {
		return nil, errors.New("storage barrier: invalid instance key or generation")
	}
	if parent != nil && parent.op.service == s && parent.op.key == key {
		parent.op.mu.Lock()
		if !parent.closed {
			if parent.op.generation != generation {
				parent.op.mu.Unlock()
				return nil, ErrStale
			}
			parent.op.refs++
			parent.op.mu.Unlock()
			return &Permit{op: parent.op}, nil
		}
		parent.op.mu.Unlock()
	}
	token := "operation:" + utils.RandomString(32)
	if _, err := s.operation(ctx, key, "enter", token, generation); err != nil {
		return nil, err
	}
	return &Permit{op: &operation{service: s, key: key, token: token, generation: generation, refs: 1}}, nil
}

func (p *Permit) Close() error {
	p.once.Do(func() {
		p.op.mu.Lock()
		defer p.op.mu.Unlock()
		p.closed = true
		p.op.refs--
		if p.op.refs == 0 {
			_, p.err = p.op.service.operation(context.Background(), p.op.key, "leave", p.op.token, 0)
		}
	})
	return p.err
}

// Guard owns closed admission. It does not expire, including after a process
// crash. Only this owner may reopen admission.
type Guard struct {
	service    *Service
	key, token string
}

func (s *Service) Block(ctx context.Context, key string, generation int64) (*Guard, error) {
	if key == "" || generation < 0 {
		return nil, errors.New("storage barrier: invalid instance key or generation")
	}
	g := &Guard{service: s, key: key, token: utils.RandomString(32)}
	if _, err := s.operation(ctx, key, "close", g.token, generation); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Guard) Wait(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		n, err := g.service.operation(ctx, g.key, "check", g.token, 0)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (g *Guard) Check(ctx context.Context) error {
	n, err := g.service.operation(ctx, g.key, "check", g.token, 0)
	if err != nil {
		return err
	}
	if n != 0 {
		return ErrBusy
	}
	return nil
}

// Open resumes admission only when every admitted operation has finished.
// Persist the new backend and generation before calling it. A failed or
// ambiguous backend update must leave the guard closed for operator recovery.
func (g *Guard) Open(ctx context.Context, generation int64) error {
	if generation < 0 {
		return ErrStale
	}
	_, err := g.service.operation(ctx, g.key, "open", g.token, generation)
	return err
}

// Cancel reopens admission without changing generation, even if operations are
// still draining. Use only before modifying the backend or its contents.
func (g *Guard) Cancel(ctx context.Context) error {
	_, err := g.service.operation(ctx, g.key, "cancel", g.token, 0)
	return err
}
