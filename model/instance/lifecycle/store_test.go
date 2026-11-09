package lifecycle

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMagicLinkStore(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = &memStore{vals: make(map[string]time.Time)}
			if backend == "redis" {
				if testing.Short() {
					t.Skip("Redis is required")
				}
				client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
				t.Cleanup(func() { _ = client.Close() })
				store = &redisStore{c: client, ctx: context.Background()}
			}
			alice := prefixer.NewPrefixer(0, "alice.example", t.Name()+"-alice")
			bob := prefixer.NewPrefixer(0, "bob.example", t.Name()+"-bob")
			const code = "magic-link-test"
			require.NoError(t, store.SaveMagicLinkCode(alice, code))
			assert.False(t, store.CheckMagicLinkCode(bob, code))
			assert.False(t, store.ConsumeMagicLinkCode(bob, code))
			assert.True(t, store.CheckMagicLinkCode(alice, code))
			assert.True(t, store.CheckMagicLinkCode(alice, code), "the password form must not spend the code")

			var successes atomic.Int32
			var wg sync.WaitGroup
			for range 16 {
				wg.Go(func() {
					if store.ConsumeMagicLinkCode(alice, code) {
						successes.Add(1)
					}
				})
			}
			wg.Wait()
			assert.EqualValues(t, 1, successes.Load())
			assert.False(t, store.CheckMagicLinkCode(alice, code))
			assert.False(t, store.ConsumeMagicLinkCode(alice, code))
		})
	}
}
