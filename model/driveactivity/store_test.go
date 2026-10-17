package driveactivity

import (
	"context"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore(t *testing.T) {
	t.Run("InMemory", func(t *testing.T) {
		testStore(t, newMemStore())
	})

	t.Run("InRedis", func(t *testing.T) {
		if testing.Short() {
			t.Skip("a redis is required for this test: test skipped due to the use of --short flag")
		}
		opt, err := redis.ParseURL("redis://localhost:6379/15")
		require.NoError(t, err)
		cli := redis.NewClient(opt)
		require.NoError(t, cli.Ping(context.Background()).Err())
		testStore(t, newRedisStore(cli))
	})
}

func testStore(t *testing.T, st store) {
	drive := "test/" + utils.RandomString(10)
	add := func(c change, window string) bool {
		t.Helper()
		opened, err := st.add(drive, c, window, time.Minute)
		require.NoError(t, err)
		return opened
	}

	empty, err := st.take(drive, "w0")
	require.NoError(t, err)
	assert.Empty(t, empty)

	assert.True(t, add(change{"alice@example.net", "dir1", Created, "a.txt"}, "w1"))
	empty, err = st.take(drive, "w0")
	require.NoError(t, err)
	assert.Empty(t, empty, "taking a window again must not take the next one")

	assert.False(t, add(change{"alice@example.net", "dir1", Created, "a.txt"}, "w2"))
	for _, name := range []string{"b.txt", "c.txt", "d.txt", "e.txt", "f.txt"} {
		add(change{"alice@example.net", "dir1", Updated, name}, "w2")
	}
	add(change{"bob@example.net", "dir1", Deleted, "z.txt"}, "w2")

	counts, err := st.take(drive, "w1")
	require.NoError(t, err)
	assert.Equal(t, map[key]*tally{
		{"alice@example.net", "dir1"}: {created: 2, updated: 5, samples: []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}},
		{"bob@example.net", "dir1"}:   {deleted: 1, samples: []string{"z.txt"}},
	}, counts)

	// A change after the take opens the next window, and a flush that runs
	// again gets the same counts.
	assert.True(t, add(change{"alice@example.net", "dir2", Created, "g.txt"}, "w3"))
	again, err := st.take(drive, "w1")
	require.NoError(t, err)
	assert.Equal(t, counts, again)

	require.NoError(t, st.drop(drive, "w1"))
	dropped, err := st.take(drive, "w1")
	require.NoError(t, err)
	assert.Empty(t, dropped)

	next, err := st.take(drive, "w3")
	require.NoError(t, err)
	assert.Equal(t, map[key]*tally{
		{"alice@example.net", "dir2"}: {created: 1, samples: []string{"g.txt"}},
	}, next)
	assert.True(t, add(change{"alice@example.net", "dir2", Created, "h.txt"}, "w4"))
}
