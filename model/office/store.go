package office

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
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
	// participants are the instance domains of every person who opened the
	// editing session, each once: the document server save callbacks credit
	// them all. Only addParticipant adds to it.
	participants []string
}

// Clone returns a deep copy of the session, which shares no memory with it.
func (d *ConflictDetector) Clone() ConflictDetector {
	cloned := *d
	cloned.MD5Sum = slices.Clone(d.MD5Sum)
	cloned.participants = slices.Clone(d.participants)
	return cloned
}

// Participants returns the instance domains of the people who opened the
// editing session.
func (d *ConflictDetector) Participants() []string {
	return slices.Clone(d.participants)
}

// addParticipant adds a person who opened the editing session, once.
func (d *ConflictDetector) addParticipant(domain string) {
	if domain != "" && !slices.Contains(d.participants, domain) {
		d.participants = append(d.participants, domain)
	}
}

// conflictDetectorJSON is the stored form of a ConflictDetector: the redis
// store keeps sessions as JSON, which skips unexported fields.
type conflictDetectorJSON struct {
	ID           string
	Rev          string
	MD5Sum       []byte
	Participants []string
}

// MarshalJSON is part of the json.Marshaler interface.
func (d ConflictDetector) MarshalJSON() ([]byte, error) {
	return json.Marshal(conflictDetectorJSON{
		ID:           d.ID,
		Rev:          d.Rev,
		MD5Sum:       d.MD5Sum,
		Participants: d.Participants(),
	})
}

// UnmarshalJSON is part of the json.Unmarshaler interface.
func (d *ConflictDetector) UnmarshalJSON(b []byte) error {
	var stored conflictDetectorJSON
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	*d = ConflictDetector{
		ID:           stored.ID,
		Rev:          stored.Rev,
		MD5Sum:       stored.MD5Sum,
		participants: stored.Participants,
	}
	return nil
}

// Store is an object to store and retrieve document server keys <-> id,rev
type Store interface {
	GetSecretByID(db prefixer.Prefixer, id string) (string, error)
	AddDoc(db prefixer.Prefixer, payload ConflictDetector) (string, error)
	GetDoc(db prefixer.Prefixer, secret string) (*ConflictDetector, error)
	UpdateDoc(db prefixer.Prefixer, secret string, payload ConflictDetector) error
	// AddParticipant adds a person who opened the editing session, without
	// losing a concurrent update of the session.
	AddParticipant(db prefixer.Prefixer, secret, domain string) error
	// MoveToRevision moves the session to a revision of a file, keeping the
	// participants added in the meantime. The file may be another one (the
	// copy made on a conflict): its index then points to the session.
	MoveToRevision(db prefixer.Prefixer, secret, id, rev string, md5sum []byte) error
	RemoveDoc(db prefixer.Prefixer, secret string) error
}

// storeUpdateAttempts bounds the optimistic transactions of the redis store
// when the same session is updated concurrently.
const storeUpdateAttempts = 3

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

func newMemStore() Store {
	store := &memStore{
		vals: make(map[string]*memRef),
		byID: make(map[string]string),
	}
	go store.cleaner()
	return store
}

type memStore struct {
	mu   sync.Mutex
	vals map[string]*memRef
	byID map[string]string // id -> secret
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
		s.mu.Unlock()
	}
}

func (s *memStore) GetSecretByID(db prefixer.Prefixer, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id], nil
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

func (s *memStore) AddParticipant(db prefixer.Prefixer, secret, domain string) error {
	return s.update(db, secret, func(val *ConflictDetector) {
		val.addParticipant(domain)
	})
}

func (s *memStore) MoveToRevision(db prefixer.Prefixer, secret, id, rev string, md5sum []byte) error {
	return s.update(db, secret, func(val *ConflictDetector) {
		val.ID = id
		val.Rev = rev
		val.MD5Sum = slices.Clone(md5sum)
	})
}

// update applies fn to a copy of the stored session under the lock, and
// stores it while the secret is still the one of its file. The file index
// follows a session moved to another file. An expired session, like an update
// that is not stored, leaves the store untouched.
func (s *memStore) update(db prefixer.Prefixer, secret string, fn func(*ConflictDetector)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := docKey(db, secret)
	ref, ok := s.vals[key]
	if !ok {
		return nil
	}
	if time.Now().After(ref.exp) {
		delete(s.vals, key)
		return nil
	}
	if s.byID[ref.val.ID] != secret {
		return nil
	}
	val := ref.val.Clone()
	fn(&val)
	if val.ID != ref.val.ID {
		delete(s.byID, ref.val.ID)
		s.byID[val.ID] = secret
	}
	s.vals[key] = &memRef{
		val: val,
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

func (s *redisStore) AddParticipant(db prefixer.Prefixer, secret, domain string) error {
	return s.update(db, secret, func(val *ConflictDetector) {
		val.addParticipant(domain)
	})
}

func (s *redisStore) MoveToRevision(db prefixer.Prefixer, secret, id, rev string, md5sum []byte) error {
	return s.update(db, secret, func(val *ConflictDetector) {
		val.ID = id
		val.Rev = rev
		val.MD5Sum = md5sum
	})
}

// update applies fn to the stored session in an optimistic transaction on its
// key, retried when a concurrent update wins, and stores it while the secret
// is still the one of its file. The file index follows a session moved to
// another file, but only after the session: in a redis cluster, the two keys
// may be on different slots, where a transaction cannot span both. Until the
// index is moved, the old file still points to the session.
func (s *redisStore) update(db prefixer.Prefixer, secret string, fn func(*ConflictDetector)) error {
	key := docKey(db, secret)
	var oldID, newID string // the file of the session before and after fn
	txf := func(tx *redis.Tx) error {
		oldID, newID = "", ""
		b, err := tx.Get(s.ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		var val ConflictDetector
		if err = json.Unmarshal(b, &val); err != nil {
			return err
		}
		owner, err := s.c.Get(s.ctx, docKey(db, val.ID)).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		if owner != secret {
			return nil
		}
		previous := val.ID
		fn(&val)
		v, err := json.Marshal(val)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(s.ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(s.ctx, key, v, storeTTL)
			return nil
		})
		if err == nil {
			oldID, newID = previous, val.ID
		}
		return err
	}

	var err error
	for attempt := 0; attempt < storeUpdateAttempts; attempt++ {
		err = s.c.Watch(s.ctx, txf, key)
		if !errors.Is(err, redis.TxFailedErr) {
			break
		}
	}
	if err != nil || newID == oldID {
		return err
	}
	if err := s.c.Set(s.ctx, docKey(db, newID), secret, storeTTL).Err(); err != nil {
		return err
	}
	return s.c.Del(s.ctx, docKey(db, oldID)).Err()
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

func docKey(db prefixer.Prefixer, suffix string) string {
	return db.DBPrefix() + ":oodoc:" + suffix
}

func makeSecret() string {
	return hex.EncodeToString(crypto.GenerateRandomBytes(12))
}
