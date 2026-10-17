package driveactivity

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/redis/go-redis/v9"
)

// The kinds of change counted.
const (
	Created = "created"
	Updated = "updated"
	Deleted = "deleted"
)

// maxSamples is the number of file names kept per member and folder.
const maxSamples = 5

// keepCounts bounds how long counts that no flush took stay in the store.
const keepCounts = 24 * time.Hour

type change struct {
	member string
	folder string
	kind   string
	name   string
}

// key is a member and the folder holding the files they changed.
type key struct {
	member string
	folder string
}

type tally struct {
	created int
	updated int
	deleted int
	samples []string
}

func (t *tally) count(kind string, n int) {
	switch kind {
	case Created:
		t.created += n
	case Updated:
		t.updated += n
	case Deleted:
		t.deleted += n
	}
}

func (t *tally) sample(name string) {
	if len(t.samples) < maxSamples && !slices.Contains(t.samples, name) {
		t.samples = append(t.samples, name)
	}
}

// store holds the counts of each drive until its window is flushed.
type store interface {
	// add counts a change. When no window is open for the drive, it opens
	// this window and returns true: the caller schedules its flush.
	add(drive string, c change, window string, open time.Duration) (bool, error)
	// take moves the counts of the drive aside for this window and returns
	// them. Taking the same window again returns the same counts, until drop.
	take(drive, window string) (map[key]*tally, error)
	// drop forgets the counts taken for this window. Taking it again
	// returns nothing, instead of the counts of the next window.
	drop(drive, window string) error
}

var (
	storeMu     sync.Mutex
	globalStore store
)

func getStore() store {
	storeMu.Lock()
	defer storeMu.Unlock()
	if globalStore != nil {
		return globalStore
	}
	if cli := config.GetConfig().SessionStorage; cli != nil {
		globalStore = newRedisStore(cli)
	} else {
		globalStore = newMemStore()
	}
	return globalStore
}

type memWindow struct {
	id  string
	exp time.Time
}

type memTaken struct {
	counts map[key]*tally
	exp    time.Time
}

type memStore struct {
	mu      sync.Mutex
	windows map[string]memWindow
	counts  map[string]map[key]*tally
	taken   map[string]memTaken
}

func newMemStore() *memStore {
	return &memStore{
		windows: make(map[string]memWindow),
		counts:  make(map[string]map[key]*tally),
		taken:   make(map[string]memTaken),
	}
}

func (s *memStore) add(drive string, c change, window string, open time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := s.counts[drive]
	if counts == nil {
		counts = make(map[key]*tally)
		s.counts[drive] = counts
	}
	k := key{c.member, c.folder}
	t := counts[k]
	if t == nil {
		t = &tally{}
		counts[k] = t
	}
	t.count(c.kind, 1)
	t.sample(c.name)

	now := time.Now()
	if w, ok := s.windows[drive]; ok && now.Before(w.exp) {
		return false, nil
	}
	s.windows[drive] = memWindow{id: window, exp: now.Add(open)}
	return true, nil
}

func (s *memStore) take(drive, window string) (map[key]*tally, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := drive + ":" + window
	if taken, ok := s.taken[id]; ok {
		return taken.counts, nil
	}
	if s.windows[drive].id == window {
		delete(s.windows, drive)
	}
	counts := s.counts[drive]
	delete(s.counts, drive)
	s.taken[id] = memTaken{counts: counts, exp: time.Now().Add(keepCounts)}
	return counts, nil
}

func (s *memStore) drop(drive, window string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, taken := range s.taken {
		if now.After(taken.exp) {
			delete(s.taken, id)
		}
	}
	s.taken[drive+":"+window] = memTaken{exp: now.Add(keepCounts)}
	return nil
}

// The counts of a drive are a hash whose fields are the member, the folder
// and what is counted, separated by newlines. The braces keep the keys of a
// drive in the same slot of a Redis cluster, as the scripts need.
type redisStore struct {
	c   redis.UniversalClient
	ctx context.Context
}

func newRedisStore(c redis.UniversalClient) *redisStore {
	return &redisStore{c, context.Background()}
}

func redisKeys(drive string, rest ...string) []string {
	prefix := "drive-activity:{" + drive + "}:"
	keys := make([]string, len(rest))
	for i, r := range rest {
		keys[i] = prefix + r
	}
	return keys
}

// KEYS: window, counts. ARGV: field prefix, kind, name, window, TTL of the
// counts, TTL of the window, number of samples.
var addScript = redis.NewScript(`
redis.call("HINCRBY", KEYS[2], ARGV[1] .. ARGV[2], 1)
local n = tonumber(redis.call("HGET", KEYS[2], ARGV[1] .. "samples") or "0")
if n < tonumber(ARGV[7]) then
  local seen = false
  for i = 1, n do
    if redis.call("HGET", KEYS[2], ARGV[1] .. "sample" .. i) == ARGV[3] then seen = true end
  end
  if not seen then
    redis.call("HSET", KEYS[2], ARGV[1] .. "sample" .. (n + 1), ARGV[3], ARGV[1] .. "samples", n + 1)
  end
end
redis.call("EXPIRE", KEYS[2], ARGV[5])
if redis.call("SET", KEYS[1], ARGV[4], "NX", "EX", ARGV[6]) then return 1 end
return 0
`)

// KEYS: window, counts, taken. ARGV: window, TTL of the taken counts.
var takeScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[3]) == 0 then
  if redis.call("GET", KEYS[1]) == ARGV[1] then redis.call("DEL", KEYS[1]) end
  if redis.call("EXISTS", KEYS[2]) == 1 then
    redis.call("RENAME", KEYS[2], KEYS[3])
  end
  redis.call("HSET", KEYS[3], "taken", 1)
  redis.call("EXPIRE", KEYS[3], ARGV[2])
end
return redis.call("HGETALL", KEYS[3])
`)

func (s *redisStore) add(drive string, c change, window string, open time.Duration) (bool, error) {
	prefix := c.member + "\n" + c.folder + "\n"
	opened, err := addScript.Run(s.ctx, s.c, redisKeys(drive, "window", "counts"),
		prefix, c.kind, c.name, window, seconds(keepCounts), seconds(open), maxSamples).Int()
	return opened == 1, err
}

func (s *redisStore) take(drive, window string) (map[key]*tally, error) {
	fields, err := takeScript.Run(s.ctx, s.c, redisKeys(drive, "window", "counts", window),
		window, seconds(keepCounts)).StringSlice()
	if err != nil {
		return nil, err
	}
	return parseCounts(fields), nil
}

func (s *redisStore) drop(drive, window string) error {
	k := redisKeys(drive, window)[0]
	pipe := s.c.TxPipeline()
	pipe.Del(s.ctx, k)
	pipe.HSet(s.ctx, k, "dropped", 1)
	pipe.Expire(s.ctx, k, keepCounts)
	_, err := pipe.Exec(s.ctx)
	return err
}

func parseCounts(fields []string) map[key]*tally {
	counts := make(map[key]*tally)
	samples := make(map[key][]string)
	for i := 0; i+1 < len(fields); i += 2 {
		parts := strings.SplitN(fields[i], "\n", 3)
		if len(parts) != 3 {
			continue
		}
		k := key{parts[0], parts[1]}
		t := counts[k]
		if t == nil {
			t = &tally{}
			counts[k] = t
		}
		what, value := parts[2], fields[i+1]
		if idx, ok := strings.CutPrefix(what, "sample"); ok && idx != "s" {
			n, err := strconv.Atoi(idx)
			if err != nil || n < 1 || n > maxSamples {
				continue
			}
			if samples[k] == nil {
				samples[k] = make([]string, maxSamples)
			}
			samples[k][n-1] = value
			continue
		}
		if n, err := strconv.Atoi(value); err == nil {
			t.count(what, n)
		}
	}
	for k, names := range samples {
		for _, name := range names {
			if name != "" {
				counts[k].sample(name)
			}
		}
	}
	return counts
}

func seconds(d time.Duration) int64 {
	return max(1, int64(d/time.Second))
}
