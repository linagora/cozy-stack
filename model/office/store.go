package office

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
)

type ConflictDetector struct {
	ID     string
	Rev    string
	MD5Sum []byte
}

// Clone returns a deep copy of the session, which shares no memory with it.
func (d *ConflictDetector) Clone() ConflictDetector {
	cloned := *d
	cloned.MD5Sum = slices.Clone(d.MD5Sum)
	return cloned
}

// Store is an object to store and retrieve document server keys <-> id,rev
type Store interface {
	GetSecretByID(db prefixer.Prefixer, id string) (string, error)
	UpdateSecret(db prefixer.Prefixer, secret, oldID, newID string) error
	AddDoc(db prefixer.Prefixer, payload ConflictDetector) (string, error)
	GetDoc(db prefixer.Prefixer, secret string) (*ConflictDetector, error)
	UpdateDoc(db prefixer.Prefixer, secret string, payload ConflictDetector) error
	RemoveDoc(db prefixer.Prefixer, secret string) error

	// AcquireForceSave takes the forcesave of the session of the key for the
	// owner, until it is finished or for ttl. It reports false when a
	// forcesave is running or has ended within its delay.
	AcquireForceSave(db prefixer.Prefixer, secret, owner string, ttl time.Duration) (bool, error)
	// FinishForceSave records the outcome of the forcesave of the owner for
	// delay, which also frees it. It does nothing when the owner no longer
	// holds the forcesave.
	FinishForceSave(db prefixer.Prefixer, secret, owner string, outcome ForceSaveOutcome, delay time.Duration) error
	// LastForceSave returns the outcome of the forcesave of the session ended
	// within its delay, or nil.
	LastForceSave(db prefixer.Prefixer, secret string) (*ForceSaveOutcome, error)
}

// ForceSaveOutcome is how a forcesave has ended, for the readers of the
// document on all the stacks.
type ForceSaveOutcome struct {
	Saved bool   `json:"saved"`
	Error string `json:"error,omitempty"`
}

func (o *ForceSaveOutcome) err() error {
	if o.Error == "" {
		return nil
	}
	return errors.New(o.Error)
}

// storeTTL is the time an entry stay alive
var storeTTL = 30 * 24 * time.Hour

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

type memRef struct {
	val ConflictDetector
	exp time.Time
}

// memForceSave is the forcesave of a session: running for its owner, or
// ended with its outcome.
type memForceSave struct {
	owner   string
	outcome *ForceSaveOutcome
	exp     time.Time
}

func newMemStore() Store {
	store := &memStore{
		vals:       make(map[string]*memRef),
		byID:       make(map[string]string),
		forceSaves: make(map[string]memForceSave),
	}
	go store.cleaner()
	return store
}

type memStore struct {
	mu         sync.Mutex
	vals       map[string]*memRef
	byID       map[string]string // id -> secret
	forceSaves map[string]memForceSave
}

func (s *memStore) cleaner() {
	for range time.Tick(storeCleanInterval) {
		s.mu.Lock()
		now := time.Now()
		for k, v := range s.vals {
			if now.After(v.exp) {
				if s.byID[v.val.ID] == k {
					delete(s.byID, v.val.ID)
				}
				delete(s.vals, k)
			}
		}
		for k, v := range s.forceSaves {
			if now.After(v.exp) {
				delete(s.forceSaves, k)
			}
		}
		s.mu.Unlock()
	}
}

func (s *memStore) GetSecretByID(db prefixer.Prefixer, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id], nil
}

func (s *memStore) UpdateSecret(db prefixer.Prefixer, secret, oldID, newID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, oldID)
	s.byID[newID] = secret
	return nil
}

func (s *memStore) AddDoc(db prefixer.Prefixer, payload ConflictDetector) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	secret := makeSecret()
	s.byID[payload.ID] = secret
	key := docKey(db, secret)
	s.vals[key] = &memRef{
		val: payload.Clone(),
		exp: time.Now().Add(storeTTL),
	}
	return secret, nil
}

func (s *memStore) GetDoc(db prefixer.Prefixer, secret string) (*ConflictDetector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := docKey(db, secret)
	ref, ok := s.vals[key]
	if !ok {
		return nil, nil
	}
	if time.Now().After(ref.exp) {
		delete(s.vals, key)
		return nil, nil
	}
	// A copy: the stored session is only changed under the lock.
	cloned := ref.val.Clone()
	return &cloned, nil
}

func (s *memStore) UpdateDoc(db prefixer.Prefixer, secret string, payload ConflictDetector) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID[payload.ID] != secret {
		return nil
	}
	key := docKey(db, secret)
	s.vals[key] = &memRef{
		val: payload.Clone(),
		exp: time.Now().Add(storeTTL),
	}
	return nil
}

func (s *memStore) RemoveDoc(db prefixer.Prefixer, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := docKey(db, secret)
	if v, ok := s.vals[key]; ok {
		if s.byID[v.val.ID] == secret {
			delete(s.byID, v.val.ID)
		}
	}
	delete(s.vals, key)
	return nil
}

// forceSave returns the forcesave of the session, if it has not expired. The
// caller holds the lock.
func (s *memStore) forceSave(key string) (memForceSave, bool) {
	fs, ok := s.forceSaves[key]
	if ok && time.Now().After(fs.exp) {
		delete(s.forceSaves, key)
		return fs, false
	}
	return fs, ok
}

func (s *memStore) AcquireForceSave(db prefixer.Prefixer, secret, owner string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := forceSaveKey(db, secret)
	if _, ok := s.forceSave(key); ok {
		return false, nil
	}
	s.forceSaves[key] = memForceSave{owner: owner, exp: time.Now().Add(ttl)}
	return true, nil
}

func (s *memStore) FinishForceSave(db prefixer.Prefixer, secret, owner string, outcome ForceSaveOutcome, delay time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := forceSaveKey(db, secret)
	if fs, ok := s.forceSave(key); !ok || fs.outcome != nil || fs.owner != owner {
		return nil
	}
	if delay <= 0 {
		delete(s.forceSaves, key)
		return nil
	}
	s.forceSaves[key] = memForceSave{outcome: &outcome, exp: time.Now().Add(delay)}
	return nil
}

func (s *memStore) LastForceSave(db prefixer.Prefixer, secret string) (*ForceSaveOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, ok := s.forceSave(forceSaveKey(db, secret))
	if !ok || fs.outcome == nil {
		return nil, nil
	}
	outcome := *fs.outcome
	return &outcome, nil
}

type redisStore struct {
	c   redis.UniversalClient
	ctx context.Context
}

func (s *redisStore) GetSecretByID(db prefixer.Prefixer, id string) (string, error) {
	idKey := docKey(db, id)
	res, err := s.c.Get(s.ctx, idKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return res, err
}

func (s *redisStore) UpdateSecret(db prefixer.Prefixer, secret, oldID, newID string) error {
	_ = s.c.Del(s.ctx, docKey(db, oldID))
	idKey := docKey(db, newID)
	return s.c.Set(s.ctx, idKey, secret, storeTTL).Err()
}

func (s *redisStore) AddDoc(db prefixer.Prefixer, payload ConflictDetector) (string, error) {
	idKey := docKey(db, payload.ID)
	v, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	secret := makeSecret()
	key := docKey(db, secret)
	if err = s.c.Set(s.ctx, key, v, storeTTL).Err(); err != nil {
		return "", err
	}
	if err = s.c.Set(s.ctx, idKey, secret, storeTTL).Err(); err != nil {
		return "", err
	}
	return secret, nil
}

func (s *redisStore) GetDoc(db prefixer.Prefixer, secret string) (*ConflictDetector, error) {
	key := docKey(db, secret)
	b, err := s.c.Get(s.ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var val ConflictDetector
	if err = json.Unmarshal(b, &val); err != nil {
		return nil, err
	}
	return &val, nil
}

func (s *redisStore) UpdateDoc(db prefixer.Prefixer, secret string, payload ConflictDetector) error {
	idKey := docKey(db, payload.ID)
	result, err := s.c.Get(s.ctx, idKey).Result()
	if err != nil {
		return err
	}
	if result != secret {
		return nil
	}
	v, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	key := docKey(db, secret)
	if err = s.c.Set(s.ctx, key, v, storeTTL).Err(); err != nil {
		return err
	}
	return nil
}

func (s *redisStore) RemoveDoc(db prefixer.Prefixer, secret string) error {
	payload, _ := s.GetDoc(db, secret)
	if payload != nil {
		idKey := docKey(db, payload.ID)
		if result, err := s.c.Get(s.ctx, idKey).Result(); err == nil && result == secret {
			_ = s.c.Del(s.ctx, idKey)
		}
	}
	key := docKey(db, secret)
	return s.c.Del(s.ctx, key).Err()
}

// The key of the forcesave of a session holds "running:<owner>" until the
// owner records "done:<outcome>" in its place.
const (
	forceSaveRunning = "running:"
	forceSaveDone    = "done:"
)

// luaFinishForceSave replaces the forcesave of the owner by its outcome, in one
// step: a reader never sees it freed without its outcome.
var luaFinishForceSave = redis.NewScript(`
if redis.call("get", KEYS[1]) ~= ARGV[1] then return 0 end
if ARGV[3] == "0" then return redis.call("del", KEYS[1]) end
return redis.call("set", KEYS[1], ARGV[2], "px", ARGV[3])`)

func (s *redisStore) AcquireForceSave(db prefixer.Prefixer, secret, owner string, ttl time.Duration) (bool, error) {
	return s.c.SetNX(s.ctx, forceSaveKey(db, secret), forceSaveRunning+owner, ttl).Result()
}

func (s *redisStore) FinishForceSave(db prefixer.Prefixer, secret, owner string, outcome ForceSaveOutcome, delay time.Duration) error {
	v, err := json.Marshal(outcome)
	if err != nil {
		return err
	}
	keys := []string{forceSaveKey(db, secret)}
	return luaFinishForceSave.Run(s.ctx, s.c, keys,
		forceSaveRunning+owner, forceSaveDone+string(v), max(delay.Milliseconds(), 0)).Err()
}

func (s *redisStore) LastForceSave(db prefixer.Prefixer, secret string) (*ForceSaveOutcome, error) {
	v, err := s.c.Get(s.ctx, forceSaveKey(db, secret)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	done, ok := strings.CutPrefix(v, forceSaveDone)
	if !ok {
		return nil, nil
	}
	var outcome ForceSaveOutcome
	if err := json.Unmarshal([]byte(done), &outcome); err != nil {
		return nil, err
	}
	return &outcome, nil
}

func forceSaveKey(db prefixer.Prefixer, secret string) string {
	return db.DBPrefix() + ":ooforcesave:" + secret
}

func docKey(db prefixer.Prefixer, suffix string) string {
	return db.DBPrefix() + ":oodoc:" + suffix
}

func makeSecret() string {
	return hex.EncodeToString(crypto.GenerateRandomBytes(12))
}
