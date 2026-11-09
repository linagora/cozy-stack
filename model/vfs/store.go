package vfs

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
)

// Store is essentially a place to store transient objects between two HTTP
// requests: it can be a secret for downloading a file, a list of files in an
// archive, or a metadata object for an upcoming upload.
type Store interface {
	AddFile(db prefixer.Prefixer, filePath string) (string, error)
	AddThumb(db prefixer.Prefixer, fileID string) (string, error)
	AddThumbs(db prefixer.Prefixer, fileIDs []string) (map[string]string, error)
	AddVersion(db prefixer.Prefixer, versionID string) (string, error)
	AddArchive(db prefixer.Prefixer, archive *Archive) (string, error)
	AddMetadata(db prefixer.Prefixer, metadata *Metadata) (string, error)
	GetFile(db prefixer.Prefixer, key string) (string, error)
	GetThumb(db prefixer.Prefixer, key string) (string, error)
	GetVersion(db prefixer.Prefixer, key string) (string, error)
	GetArchive(db prefixer.Prefixer, key string) (*Archive, error)
	GetMetadata(db prefixer.Prefixer, key string) (*Metadata, error)
}

// storeTTL is time after which the data in the store will be considered stale.
var storeTTL = 10 * time.Minute

// storeCleanInterval is the time interval between each download cleanup.
var storeCleanInterval = 1 * time.Hour

var globalStoreMu sync.Mutex
var globalStore Store

// GetStore returns the global Store.
func GetStore() Store {
	globalStoreMu.Lock()
	defer globalStoreMu.Unlock()
	if globalStore != nil {
		return globalStore
	}
	cli := config.GetConfig().DownloadStorage
	if cli == nil {
		globalStore = newMemStore()
	} else {
		globalStore = newRedisStore(cli)
	}
	return globalStore
}

func newMemStore() Store {
	store := &memStore{vals: make(map[string]*memRef)}
	go store.cleaner()
	return store
}

type memStore struct {
	mu   sync.Mutex
	vals map[string]*memRef
}

type memRef struct {
	val interface{}
	exp time.Time
}

func (s *memStore) cleaner() {
	for range time.Tick(storeCleanInterval) {
		now := time.Now()
		for k, v := range s.vals {
			if now.After(v.exp) {
				delete(s.vals, k)
			}
		}
	}
}

func (s *memStore) AddFile(db prefixer.Prefixer, filePath string) (string, error) {
	key := makeSecret()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[db.DBPrefix()+":"+key] = &memRef{
		val: filePath,
		exp: time.Now().Add(storeTTL),
	}
	return key, nil
}

func (s *memStore) AddThumb(db prefixer.Prefixer, fileID string) (string, error) {
	key := makeSecret()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[db.DBPrefix()+":"+key] = &memRef{
		val: fileID,
		exp: time.Now().Add(storeTTL),
	}
	return key, nil
}

func (s *memStore) AddThumbs(db prefixer.Prefixer, fileIDs []string) (map[string]string, error) {
	secrets := make(map[string]string)
	for _, fileID := range fileIDs {
		secret, err := s.AddThumb(db, fileID)
		if err != nil {
			return nil, err
		}
		secrets[fileID] = secret
	}
	return secrets, nil
}

func (s *memStore) AddVersion(db prefixer.Prefixer, versionID string) (string, error) {
	key := makeSecret()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[db.DBPrefix()+":"+key] = &memRef{
		val: versionID,
		exp: time.Now().Add(storeTTL),
	}
	return key, nil
}

func (s *memStore) AddArchive(db prefixer.Prefixer, archive *Archive) (string, error) {
	key := makeSecret()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[db.DBPrefix()+":"+key] = &memRef{
		val: archive,
		exp: time.Now().Add(storeTTL),
	}
	return key, nil
}

func (s *memStore) AddMetadata(db prefixer.Prefixer, metadata *Metadata) (string, error) {
	key := makeSecret()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[db.DBPrefix()+":"+key] = &memRef{
		val: metadata,
		exp: time.Now().Add(storeTTL),
	}
	return key, nil
}

func (s *memStore) GetFile(db prefixer.Prefixer, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = db.DBPrefix() + ":" + key
	ref, ok := s.vals[key]
	if !ok {
		return "", ErrWrongToken
	}
	if time.Now().After(ref.exp) {
		delete(s.vals, key)
		return "", ErrWrongToken
	}
	f, ok := ref.val.(string)
	if !ok {
		return "", ErrWrongToken
	}
	return f, nil
}

func (s *memStore) GetThumb(db prefixer.Prefixer, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = db.DBPrefix() + ":" + key
	ref, ok := s.vals[key]
	if !ok {
		return "", ErrWrongToken
	}
	if time.Now().After(ref.exp) {
		delete(s.vals, key)
		return "", ErrWrongToken
	}
	f, ok := ref.val.(string)
	if !ok {
		return "", ErrWrongToken
	}
	return f, nil
}

func (s *memStore) GetVersion(db prefixer.Prefixer, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = db.DBPrefix() + ":" + key
	ref, ok := s.vals[key]
	if !ok {
		return "", ErrWrongToken
	}
	if time.Now().After(ref.exp) {
		delete(s.vals, key)
		return "", ErrWrongToken
	}
	f, ok := ref.val.(string)
	if !ok {
		return "", ErrWrongToken
	}
	return f, nil
}

func (s *memStore) GetArchive(db prefixer.Prefixer, key string) (*Archive, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = db.DBPrefix() + ":" + key
	ref, ok := s.vals[key]
	if !ok {
		return nil, ErrWrongToken
	}
	if time.Now().After(ref.exp) {
		delete(s.vals, key)
		return nil, ErrWrongToken
	}
	a, ok := ref.val.(*Archive)
	if !ok {
		return nil, ErrWrongToken
	}
	return a, nil
}

func (s *memStore) GetMetadata(db prefixer.Prefixer, key string) (*Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = db.DBPrefix() + ":" + key
	ref, ok := s.vals[key]
	if !ok {
		return nil, ErrInvalidMetadataID
	}
	if time.Now().After(ref.exp) {
		delete(s.vals, key)
		return nil, ErrInvalidMetadataID
	}
	m, ok := ref.val.(*Metadata)
	if !ok {
		return nil, ErrInvalidMetadataID
	}
	return m, nil
}

type redisStore struct {
	c   redis.UniversalClient
	ctx context.Context
}

func newRedisStore(cli redis.UniversalClient) Store {
	ctx := context.Background()
	return &redisStore{cli, ctx}
}

func (s *redisStore) AddFile(db prefixer.Prefixer, filePath string) (string, error) {
	key := makeSecret()
	if err := s.c.Set(s.ctx, db.DBPrefix()+":"+key, filePath, storeTTL).Err(); err != nil {
		return "", err
	}
	return key, nil
}

func (s *redisStore) AddThumb(db prefixer.Prefixer, fileID string) (string, error) {
	key := makeSecret()
	if err := s.c.Set(s.ctx, db.DBPrefix()+":"+key, fileID, storeTTL).Err(); err != nil {
		return "", err
	}
	return key, nil
}

func (s *redisStore) AddThumbs(db prefixer.Prefixer, fileIDs []string) (map[string]string, error) {
	secrets := make(map[string]string)
	pipe := s.c.Pipeline()
	for _, fileID := range fileIDs {
		key := makeSecret()
		secrets[fileID] = key
		pipe.Set(s.ctx, db.DBPrefix()+":"+key, fileID, storeTTL)
	}
	if _, err := pipe.Exec(s.ctx); err != nil {
		return nil, err
	}
	return secrets, nil
}

func (s *redisStore) AddVersion(db prefixer.Prefixer, versionID string) (string, error) {
	key := makeSecret()
	if err := s.c.Set(s.ctx, db.DBPrefix()+":"+key, versionID, storeTTL).Err(); err != nil {
		return "", err
	}
	return key, nil
}

func (s *redisStore) AddArchive(db prefixer.Prefixer, archive *Archive) (string, error) {
	v, err := json.Marshal(archive)
	if err != nil {
		return "", err
	}
	key := makeSecret()
	if err = s.c.Set(s.ctx, db.DBPrefix()+":"+key, v, storeTTL).Err(); err != nil {
		return "", err
	}
	return key, nil
}

func (s *redisStore) AddMetadata(db prefixer.Prefixer, metadata *Metadata) (string, error) {
	v, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	key := makeSecret()
	if err = s.c.Set(s.ctx, db.DBPrefix()+":"+key, v, storeTTL).Err(); err != nil {
		return "", err
	}
	return key, nil
}

func (s *redisStore) GetFile(db prefixer.Prefixer, key string) (string, error) {
	f, err := s.c.Get(s.ctx, db.DBPrefix()+":"+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrWrongToken
	}
	if err != nil {
		return "", err
	}
	return f, nil
}

func (s *redisStore) GetThumb(db prefixer.Prefixer, key string) (string, error) {
	f, err := s.c.Get(s.ctx, db.DBPrefix()+":"+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrWrongToken
	}
	if err != nil {
		return "", err
	}
	return f, nil
}

func (s *redisStore) GetVersion(db prefixer.Prefixer, key string) (string, error) {
	f, err := s.c.Get(s.ctx, db.DBPrefix()+":"+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrWrongToken
	}
	if err != nil {
		return "", err
	}
	return f, nil
}

func (s *redisStore) GetArchive(db prefixer.Prefixer, key string) (*Archive, error) {
	b, err := s.c.Get(s.ctx, db.DBPrefix()+":"+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrWrongToken
	}
	if err != nil {
		return nil, err
	}
	arch := &Archive{}
	if err = json.Unmarshal(b, arch); err != nil {
		return nil, err
	}
	return arch, nil
}

func (s *redisStore) GetMetadata(db prefixer.Prefixer, key string) (*Metadata, error) {
	b, err := s.c.Get(s.ctx, db.DBPrefix()+":"+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrInvalidMetadataID
	}
	if err != nil {
		return nil, err
	}
	meta := &Metadata{}
	if err = json.Unmarshal(b, meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func makeSecret() string {
	return hex.EncodeToString(crypto.GenerateRandomBytes(8))
}
