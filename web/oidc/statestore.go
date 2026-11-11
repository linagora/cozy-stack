package oidc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	oidcprovider "github.com/cozy/cozy-stack/model/oidc/provider"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/redis/go-redis/v9"
)

const (
	stateTTL = 15 * time.Minute
	codeTTL  = 10 * time.Minute
)

type stateHolder struct {
	id           string
	expiresAt    int64
	Provider     oidcprovider.Kind
	Instance     string
	Redirect     string
	Nonce        string
	Confirm      string
	OIDCContext  string
	SharingID    string
	SharingState string
}

func newStateHolder(domain, redirect, confirm, oidcContext string, provider oidcprovider.Kind) *stateHolder {
	id := hex.EncodeToString(crypto.GenerateRandomBytes(24))
	nonce := hex.EncodeToString(crypto.GenerateRandomBytes(24))
	return &stateHolder{
		id:          id,
		Provider:    provider,
		Instance:    domain,
		Redirect:    redirect,
		Confirm:     confirm,
		Nonce:       nonce,
		OIDCContext: oidcContext,
	}
}

func newSharingStateHolder(domain, sharingID, sharingState string, provider oidcprovider.Kind, contextName string) *stateHolder {
	id := hex.EncodeToString(crypto.GenerateRandomBytes(24))
	nonce := hex.EncodeToString(crypto.GenerateRandomBytes(24))
	return &stateHolder{
		id:           id,
		Provider:     provider,
		Instance:     domain,
		Nonce:        nonce,
		SharingID:    sharingID,
		SharingState: sharingState,
		OIDCContext:  contextName,
	}
}

// DelegatedCodeData holds the data associated with a delegated code
type DelegatedCodeData struct {
	Sub         string            `json:"sub"`
	SessionID   string            `json:"session_id,omitempty"`
	ContextName string            `json:"context"`
	Provider    oidcprovider.Kind `json:"provider"`
}

type stateStorage interface {
	Add(*stateHolder) error
	Find(id string) *stateHolder
	CreateCodeData(sub, sessionID, contextName string, provider oidcprovider.Kind) string
	GetCodeData(code string) *DelegatedCodeData
	DeleteCodeData(code string) bool
}

type memStateStorage struct {
	states map[string]*stateHolder
	codes  map[string]*DelegatedCodeData // delegated code -> code data
}

func (store memStateStorage) Add(state *stateHolder) error {
	state.expiresAt = time.Now().UTC().Add(stateTTL).Unix()
	store.states[state.id] = state
	return nil
}

func (store memStateStorage) Find(id string) *stateHolder {
	state, ok := store.states[id]
	if !ok {
		return nil
	}
	if state.expiresAt < time.Now().UTC().Unix() {
		delete(store.states, id)
		return nil
	}
	return state
}

func (store memStateStorage) CreateCodeData(sub, sessionID, contextName string, provider oidcprovider.Kind) string {
	code := makeCode()
	store.codes[code] = &DelegatedCodeData{Sub: sub, SessionID: sessionID, ContextName: contextName, Provider: provider}
	return code
}

func (store memStateStorage) GetCodeData(code string) *DelegatedCodeData {
	return store.codes[code]
}

func (store memStateStorage) DeleteCodeData(code string) bool {
	_, ok := store.codes[code]
	delete(store.codes, code)
	return ok
}

type subRedisInterface interface {
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
}

type redisStateStorage struct {
	cl  subRedisInterface
	ctx context.Context
}

func (store *redisStateStorage) Add(s *stateHolder) error {
	serialized, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return store.cl.Set(store.ctx, s.id, serialized, stateTTL).Err()
}

func (store *redisStateStorage) Find(id string) *stateHolder {
	serialized, err := store.cl.Get(store.ctx, id).Bytes()
	if err != nil {
		return nil
	}
	var s stateHolder
	err = json.Unmarshal(serialized, &s)
	if err != nil {
		logger.WithNamespace("redis-state").Errorf(
			"Bad state in redis %s", string(serialized))
		return nil
	}
	return &s
}

func (store *redisStateStorage) CreateCodeData(sub, sessionID, contextName string, provider oidcprovider.Kind) string {
	code := makeCode()
	data := &DelegatedCodeData{Sub: sub, SessionID: sessionID, ContextName: contextName, Provider: provider}
	serialized, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	store.cl.Set(store.ctx, code, serialized, codeTTL)
	return code
}

func (store *redisStateStorage) GetCodeData(code string) *DelegatedCodeData {
	val := store.cl.Get(store.ctx, code).Val()
	if val == "" {
		return nil
	}
	var data DelegatedCodeData
	if err := json.Unmarshal([]byte(val), &data); err != nil {
		return nil
	}
	return &data
}

// DeleteCodeData reports whether this call is the one that removed the code.
func (store *redisStateStorage) DeleteCodeData(code string) bool {
	return store.cl.Del(store.ctx, code).Val() > 0
}

var globalStorage stateStorage
var globalStorageMutex sync.Mutex

func getStorage() stateStorage {
	globalStorageMutex.Lock()
	defer globalStorageMutex.Unlock()
	if globalStorage != nil {
		return globalStorage
	}
	cli := config.GetConfig().OauthStateStorage
	if cli == nil {
		globalStorage = &memStateStorage{
			states: make(map[string]*stateHolder),
			codes:  make(map[string]*DelegatedCodeData),
		}
	} else {
		ctx := context.Background()
		globalStorage = &redisStateStorage{cl: cli, ctx: ctx}
	}
	return globalStorage
}

func makeCode() string {
	return hex.EncodeToString(crypto.GenerateRandomBytes(12))
}
