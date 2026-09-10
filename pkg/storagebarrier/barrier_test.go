package storagebarrier

import (
	"context"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestMemoryBarrier(t *testing.T) {
	s := New(nil)
	testBarrier(t, s, s)
}

func TestRedisBarrier(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Redis")
	}
	address := os.Getenv("COZY_TEST_REDIS_URL")
	if address == "" {
		address = "redis://localhost:6379/0"
	}
	opts, err := redis.ParseURL(address)
	require.NoError(t, err)
	one, two := redis.NewClient(opts), redis.NewClient(opts)
	t.Cleanup(func() { _ = one.Close(); _ = two.Close() })
	require.NoError(t, one.Ping(context.Background()).Err())
	testBarrier(t, New(one), New(two))

	t.Run("lost_owner_cannot_complete", func(t *testing.T) {
		key := "barrier-test-" + utils.RandomString(16)
		g, err := New(one).Block(context.Background(), key, 0)
		require.NoError(t, err)
		require.NoError(t, two.Del(context.Background(), "storage-barrier:"+key).Err())
		require.ErrorIs(t, g.Check(context.Background()), ErrLost)
		require.ErrorIs(t, g.Open(context.Background(), 1), ErrLost)
		require.ErrorIs(t, g.Cancel(context.Background()), ErrLost)
	})

	t.Run("connection_failure_prevents_cutover", func(t *testing.T) {
		client := redis.NewClient(opts)
		key := "barrier-test-" + utils.RandomString(16)
		t.Cleanup(func() { _ = one.Del(context.Background(), "storage-barrier:"+key).Err() })
		g, err := New(client).Block(context.Background(), key, 0)
		require.NoError(t, err)
		require.NoError(t, client.Close())
		require.Error(t, g.Check(context.Background()))
		require.Error(t, g.Open(context.Background(), 1))
		_, err = New(two).Enter(context.Background(), key, 0, nil)
		require.ErrorIs(t, err, ErrBusy)
	})
}

func testBarrier(t *testing.T, one, two *Service) {
	t.Helper()
	ctx := context.Background()
	keyFor := func(t *testing.T) string {
		key := "barrier-test-" + utils.RandomString(16)
		if one.client != nil {
			t.Cleanup(func() { require.NoError(t, one.client.Del(ctx, "storage-barrier:"+key).Err()) })
		}
		return key
	}

	t.Run("drain_upload_before_cutover", func(t *testing.T) {
		key := keyFor(t)
		upload, err := one.Enter(ctx, key, 0, nil)
		require.NoError(t, err)
		another, err := two.Enter(ctx, key, 0, nil)
		require.NoError(t, err, "uploads must remain concurrent across processes")
		g, err := two.Block(ctx, key, 0)
		require.NoError(t, err)
		_, err = one.Block(ctx, key, 0)
		require.ErrorIs(t, err, ErrBusy)
		_, err = one.Enter(ctx, key, 0, nil)
		require.ErrorIs(t, err, ErrBusy)
		require.ErrorIs(t, g.Check(ctx), ErrBusy)
		require.ErrorIs(t, g.Open(ctx, 1), ErrBusy)

		otherInstance, err := one.Enter(ctx, keyFor(t), 0, nil)
		require.NoError(t, err)
		require.NoError(t, otherInstance.Close())

		waiting, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, g.Wait(waiting), context.DeadlineExceeded)
		require.NoError(t, upload.Close())
		require.NoError(t, upload.Close(), "closing twice must not release another upload")
		require.ErrorIs(t, g.Check(ctx), ErrBusy)
		require.NoError(t, another.Close())
		require.NoError(t, g.Wait(ctx))
		require.NoError(t, g.Open(ctx, 1))
		_, err = one.Enter(ctx, key, 0, nil)
		require.ErrorIs(t, err, ErrStale)
		fresh, err := one.Enter(ctx, key, 1, nil)
		require.NoError(t, err)
		require.NoError(t, fresh.Close())
		require.ErrorIs(t, g.Cancel(ctx), ErrLost, "an old owner cannot reopen a newer barrier")
	})

	t.Run("admitted_job_finishes_nested_writes", func(t *testing.T) {
		key := keyFor(t)
		job, err := one.Enter(ctx, key, 0, nil)
		require.NoError(t, err)
		g, err := two.Block(ctx, key, 0)
		require.NoError(t, err)
		_, err = one.Enter(ctx, key, 1, job)
		require.ErrorIs(t, err, ErrStale)
		upload, err := one.Enter(ctx, key, 0, job)
		require.NoError(t, err)
		require.NoError(t, job.Close())
		require.ErrorIs(t, g.Check(ctx), ErrBusy, "the nested upload still owns its permit")
		_, err = one.Enter(ctx, key, 0, job)
		require.ErrorIs(t, err, ErrBusy, "a closed parent must not admit more work")
		require.NoError(t, upload.Close())
		require.NoError(t, g.Wait(ctx))
		require.NoError(t, g.Open(ctx, 0))
	})

	t.Run("generation_comparison_is_exact", func(t *testing.T) {
		key := keyFor(t)
		g, err := one.Block(ctx, key, math.MaxInt64)
		require.NoError(t, err)
		require.ErrorIs(t, g.Open(ctx, math.MaxInt64-1), ErrStale)
		require.NoError(t, g.Cancel(ctx))
	})

	t.Run("cancel_drain_without_changing_backend", func(t *testing.T) {
		key := keyFor(t)
		p, err := one.Enter(ctx, key, 3, nil)
		require.NoError(t, err)
		g, err := two.Block(ctx, key, 3)
		require.NoError(t, err)
		require.NoError(t, g.Cancel(ctx))
		next, err := two.Enter(ctx, key, 3, nil)
		require.NoError(t, err)
		require.NoError(t, next.Close())
		require.NoError(t, p.Close())
		g, err = one.Block(ctx, key, 3)
		require.NoError(t, err)
		require.ErrorIs(t, g.Open(ctx, 2), ErrStale, "generation cannot move backwards")
		require.NoError(t, g.Open(ctx, 4))
	})

	t.Run("admission_racing_with_block", func(t *testing.T) {
		key := keyFor(t)
		start := make(chan struct{})
		permits := make(chan *Permit, 16)
		errs := make(chan error, 16)
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				<-start
				p, err := one.Enter(ctx, key, 0, nil)
				if err != nil {
					errs <- err
				} else {
					permits <- p
				}
			})
		}
		close(start)
		g, err := two.Block(ctx, key, 0)
		require.NoError(t, err)
		wg.Wait()
		close(permits)
		close(errs)
		for err := range errs {
			require.ErrorIs(t, err, ErrBusy)
		}
		for p := range permits {
			require.NoError(t, p.Close())
		}
		require.NoError(t, g.Wait(ctx))
		require.NoError(t, g.Open(ctx, 1))
	})

	t.Run("invalid_or_cancelled_admission", func(t *testing.T) {
		key := keyFor(t)
		_, err := one.Enter(ctx, "", 0, nil)
		require.Error(t, err)
		_, err = one.Block(ctx, key, -1)
		require.Error(t, err)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = one.Enter(cancelled, key, 0, nil)
		require.ErrorIs(t, err, context.Canceled)
		_, err = one.Block(cancelled, key, 0)
		require.ErrorIs(t, err, context.Canceled)
	})
}
