package office

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveredTurnsAPanicIntoAnError(t *testing.T) {
	res, err := recovered(func() (interface{}, error) {
		panic("boom")
	})()
	assert.Nil(t, res)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestTheCommandTokenExpires(t *testing.T) {
	tokens := make(chan string, 1)
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		tokens <- body.Token
		_, _ = w.Write([]byte(`{"error":4}`))
	}))
	t.Cleanup(ds.Close)
	cfg := &config.Office{OnlyOfficeURL: ds.URL + "/", InboxSecret: "inbox"}

	saving, err := requestForceSave(context.Background(), cfg, "a-key")
	require.NoError(t, err)
	assert.False(t, saving)

	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(<-tokens, claims, func(*jwt.Token) (interface{}, error) {
		return []byte("inbox"), nil
	})
	require.NoError(t, err)
	assert.Equal(t, "forcesave", claims["c"])
	assert.Equal(t, "a-key", claims["key"])
	exp, err := claims.GetExpirationTime()
	require.NoError(t, err)
	require.NotNil(t, exp)
	assert.WithinDuration(t, time.Now().Add(commandTokenTTL), exp.Time, 5*time.Second)
	iat, err := claims.GetIssuedAt()
	require.NoError(t, err)
	require.NotNil(t, iat)
}

// forceSaveStores gives the stores to test, each as seen by two stacks.
func forceSaveStores(t *testing.T) map[string][2]Store {
	mem := newMemStore()
	stores := map[string][2]Store{"Memory": {mem, mem}}
	if testing.Short() {
		return stores
	}
	stack := func() Store {
		opt, err := redis.ParseURL("redis://localhost:6379/15")
		require.NoError(t, err)
		cli := redis.NewClient(opt)
		t.Cleanup(func() { _ = cli.Close() })
		return &redisStore{cli, context.Background()}
	}
	stores["Redis"] = [2]Store{stack(), stack()}
	return stores
}

func TestForceSaveStore(t *testing.T) {
	for name, stacks := range forceSaveStores(t) {
		a, b := stacks[0], stacks[1]
		t.Run(name, func(t *testing.T) {
			db := prefixer.NewPrefixer(0, "forcesave.example.net", "forcesave-example-net")
			secret := makeSecret()

			t.Run("OneOwnerAtATime", func(t *testing.T) {
				ok, err := a.AcquireForceSave(db, secret, "owner-a", time.Minute)
				require.NoError(t, err)
				assert.True(t, ok)
				ok, err = b.AcquireForceSave(db, secret, "owner-b", time.Minute)
				require.NoError(t, err)
				assert.False(t, ok)
				last, err := b.LastForceSave(db, secret)
				require.NoError(t, err)
				assert.Nil(t, last, "a running forcesave has no outcome")
			})

			t.Run("AnotherOwnerDoesNotFinishIt", func(t *testing.T) {
				require.NoError(t, b.FinishForceSave(db, secret, "owner-b", ForceSaveOutcome{Saved: true}, time.Minute))
				last, err := a.LastForceSave(db, secret)
				require.NoError(t, err)
				assert.Nil(t, last)
				ok, err := b.AcquireForceSave(db, secret, "owner-b", time.Minute)
				require.NoError(t, err)
				assert.False(t, ok)
			})

			t.Run("TheOutcomeIsSharedForTheDelay", func(t *testing.T) {
				outcome := ForceSaveOutcome{Error: "command service returned error 3"}
				require.NoError(t, a.FinishForceSave(db, secret, "owner-a", outcome, 300*time.Millisecond))
				last, err := b.LastForceSave(db, secret)
				require.NoError(t, err)
				assert.Equal(t, &outcome, last)
				ok, err := b.AcquireForceSave(db, secret, "owner-b", time.Minute)
				require.NoError(t, err)
				assert.False(t, ok, "taken within the delay")

				time.Sleep(400 * time.Millisecond)
				last, err = b.LastForceSave(db, secret)
				require.NoError(t, err)
				assert.Nil(t, last)
				ok, err = b.AcquireForceSave(db, secret, "owner-b", 200*time.Millisecond)
				require.NoError(t, err)
				assert.True(t, ok, "free after the delay")
			})

			t.Run("AnExpiredForceSaveIsFree", func(t *testing.T) {
				time.Sleep(300 * time.Millisecond)
				ok, err := a.AcquireForceSave(db, secret, "owner-a", time.Minute)
				require.NoError(t, err)
				assert.True(t, ok)
				// The owner that has lost it records nothing.
				require.NoError(t, b.FinishForceSave(db, secret, "owner-b", ForceSaveOutcome{Saved: true}, time.Minute))
				last, err := a.LastForceSave(db, secret)
				require.NoError(t, err)
				assert.Nil(t, last)
			})

			t.Run("WithoutDelay", func(t *testing.T) {
				require.NoError(t, a.FinishForceSave(db, secret, "owner-a", ForceSaveOutcome{Saved: true}, 0))
				last, err := b.LastForceSave(db, secret)
				require.NoError(t, err)
				assert.Nil(t, last)
				ok, err := b.AcquireForceSave(db, secret, "owner-b", time.Minute)
				require.NoError(t, err)
				assert.True(t, ok)
				require.NoError(t, b.FinishForceSave(db, secret, "owner-b", ForceSaveOutcome{}, 0))
			})
		})
	}
}

// TestOneStackSendsTheCommand runs saveSession as two stacks would: without
// the singleflight group of a process, only through the store.
func TestOneStackSendsTheCommand(t *testing.T) {
	var commands atomic.Int32
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commands.Add(1)
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"error":4}`))
	}))
	t.Cleanup(ds.Close)
	cfg := &config.Office{OnlyOfficeURL: ds.URL + "/"}
	SetForceSaveDebounce(time.Second)
	t.Cleanup(func() { SetForceSaveDebounce(ForceSaveDebounce) })

	for name, stacks := range forceSaveStores(t) {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			previous := globalStore
			globalStore = stacks[0]
			mu.Unlock()
			t.Cleanup(func() {
				mu.Lock()
				globalStore = previous
				mu.Unlock()
			})
			commands.Store(0)

			inst := &instance.Instance{Domain: "forcesave.example.net", Prefix: "forcesave-" + makeSecret()}
			key, err := GetStore().AddDoc(inst, ConflictDetector{ID: "file-1", Rev: "1-abc"})
			require.NoError(t, err)

			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					saved, err := saveSession(context.Background(), inst, cfg, key)
					assert.NoError(t, err)
					assert.False(t, saved)
				}()
			}
			wg.Wait()
			assert.Equal(t, int32(1), commands.Load())

			last, err := GetStore().LastForceSave(inst, key)
			require.NoError(t, err)
			assert.Equal(t, &ForceSaveOutcome{}, last)
		})
	}
}
