package office

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func redisTestURL() string {
	if raw := os.Getenv("COZY_TEST_REDIS_URL"); raw != "" {
		return raw
	}
	return "redis://localhost:6379/0"
}

func TestStoreParticipants(t *testing.T) {
	t.Run("MemStore", func(t *testing.T) {
		testStoreParticipants(t, newMemStore())
	})

	t.Run("RedisStore", func(t *testing.T) {
		if testing.Short() {
			t.Skip("a redis is required for this test: test skipped due to the use of --short flag")
		}
		opt, err := redis.ParseURL(redisTestURL())
		require.NoError(t, err)
		testStoreParticipants(t, &redisStore{redis.NewClient(opt), context.Background()})
	})
}

func testStoreParticipants(t *testing.T, store Store) {
	db := prefixer.NewPrefixer(0, "participants.example.net", "participants-example-net")
	newSession := func(t *testing.T) string {
		t.Helper()
		key, err := store.AddDoc(db, ConflictDetector{ID: makeSecret(), Rev: "1-abc", MD5Sum: []byte{1}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.RemoveDoc(db, key) })
		return key
	}

	t.Run("SaveKeepsTheParticipants", func(t *testing.T) {
		key := newSession(t)
		detector, err := store.GetDoc(db, key)
		require.NoError(t, err)
		require.NoError(t, store.AddParticipant(db, key, "alice.example.net", false))

		// A save writes the session it read before the opening of alice.
		detector.Rev = "2-def"
		require.NoError(t, store.UpdateDoc(db, key, *detector))

		participants, err := store.GetParticipants(db, key)
		require.NoError(t, err)
		assert.Equal(t, []Participant{{Domain: "alice.example.net"}}, participants)
	})

	t.Run("OpeningKeepsTheRevision", func(t *testing.T) {
		key := newSession(t)
		detector, err := store.GetDoc(db, key)
		require.NoError(t, err)
		detector.Rev = "2-def"
		require.NoError(t, store.UpdateDoc(db, key, *detector))

		require.NoError(t, store.AddParticipant(db, key, "alice.example.net", false))

		got, err := store.GetDoc(db, key)
		require.NoError(t, err)
		assert.Equal(t, "2-def", got.Rev)
	})

	t.Run("FirstOpeningOfAPersonWins", func(t *testing.T) {
		key := newSession(t)
		require.NoError(t, store.AddParticipant(db, key, "alice.example.net", true))
		require.NoError(t, store.AddParticipant(db, key, "alice.example.net", false))

		participants, err := store.GetParticipants(db, key)
		require.NoError(t, err)
		assert.Equal(t, []Participant{{Domain: "alice.example.net", InDrive: true}}, participants)
	})

	t.Run("ConcurrentOpeningsAreAllKept", func(t *testing.T) {
		key := newSession(t)
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				assert.NoError(t, store.AddParticipant(db, key, fmt.Sprintf("user%02d.example.net", i), false))
			}()
		}
		wg.Wait()

		participants, err := store.GetParticipants(db, key)
		require.NoError(t, err)
		assert.Len(t, participants, 20)
	})

	t.Run("RemovedSessionHasNoParticipant", func(t *testing.T) {
		key := newSession(t)
		require.NoError(t, store.AddParticipant(db, key, "alice.example.net", false))
		require.NoError(t, store.RemoveDoc(db, key))

		participants, err := store.GetParticipants(db, key)
		require.NoError(t, err)
		assert.Empty(t, participants)
	})
}
