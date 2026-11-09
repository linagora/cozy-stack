package lifecycle

import (
	"context"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
)

// Store is an object to store and retrieve magic link codes.
//
// TODO: Move to [token.Service] with [token.MagicLink] namespace.
type Store interface {
	SaveMagicLinkCode(db prefixer.Prefixer, code string) error
	CheckMagicLinkCode(db prefixer.Prefixer, code string) bool
	ConsumeMagicLinkCode(db prefixer.Prefixer, code string) bool
}

// storeTTL is the time an entry stay alive
var storeTTL = 15 * time.Minute

// storeCleanInterval is the time interval between each cleanup.
var storeCleanInterval = 1 * time.Hour

var mu sync.Mutex
var globalStore Store

// GetStore returns the store for temporary move objects.
func GetStore() Store {
	mu.Lock()
	defer mu.Unlock()
	if globalStore != nil {
		return globalStore
	}
	cli := config.GetConfig().SessionStorage
	if cli == nil {
		globalStore = newMemStore()
	} else {
		ctx := context.Background()
		globalStore = &redisStore{cli, ctx}
	}
	return globalStore
}

func newMemStore() Store {
	store := &memStore{vals: make(map[string]time.Time)}
	go store.cleaner()
	return store
}

type memStore struct {
	mu   sync.Mutex
	vals map[string]time.Time // instance-scoped code -> expiration time
}

func (s *memStore) cleaner() {
	for range time.Tick(storeCleanInterval) {
		now := time.Now()
		s.mu.Lock()
		for k, v := range s.vals {
			if now.After(v) {
				delete(s.vals, k)
			}
		}
		s.mu.Unlock()
	}
}

func (s *memStore) SaveMagicLinkCode(db prefixer.Prefixer, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[magicLinkCodeKey(db, code)] = time.Now().Add(storeTTL)
	return nil
}

func (s *memStore) CheckMagicLinkCode(db prefixer.Prefixer, code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.vals[magicLinkCodeKey(db, code)]
	if !ok {
		return false
	}
	return time.Now().Before(exp)
}

func (s *memStore) ConsumeMagicLinkCode(db prefixer.Prefixer, code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := magicLinkCodeKey(db, code)
	exp, ok := s.vals[key]
	delete(s.vals, key)
	return ok && time.Now().Before(exp)
}

type redisStore struct {
	c   redis.UniversalClient
	ctx context.Context
}

func (s *redisStore) SaveMagicLinkCode(db prefixer.Prefixer, code string) error {
	key := magicLinkCodeKey(db, code)
	return s.c.Set(s.ctx, key, "1", storeTTL).Err()
}

func (s *redisStore) CheckMagicLinkCode(db prefixer.Prefixer, code string) bool {
	key := magicLinkCodeKey(db, code)
	r, err := s.c.Exists(s.ctx, key).Result()
	return err == nil && r > 0
}

func magicLinkCodeKey(db prefixer.Prefixer, suffix string) string {
	return db.DBPrefix() + ":magic_link:" + suffix
}

func (s *redisStore) ConsumeMagicLinkCode(db prefixer.Prefixer, code string) bool {
	r, err := s.c.GetDel(s.ctx, magicLinkCodeKey(db, code)).Result()
	return err == nil && r == "1"
}
