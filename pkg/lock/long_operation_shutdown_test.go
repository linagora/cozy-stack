package lock

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type shutdownLeaseStub struct {
	renewals            atomic.Int32
	unlocked            atomic.Bool
	renewalsAfterUnlock atomic.Int32
}

func (s *shutdownLeaseStub) Lock() error { s.unlocked.Store(false); return nil }
func (s *shutdownLeaseStub) Unlock()     { s.unlocked.Store(true) }
func (s *shutdownLeaseStub) Extend() {
	s.renewals.Add(1)
	if s.unlocked.Load() {
		s.renewalsAfterUnlock.Add(1)
	}
}

func TestLongOperationStopsRenewingOnUnlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &shutdownLeaseStub{}
		l := &longOperation{lock: s, timeout: 3 * time.Second}
		for range 2 {
			require.NoError(t, l.Lock())
			synctest.Wait()
			before := s.renewals.Load()
			time.Sleep(time.Second)
			synctest.Wait()
			require.Equal(t, before+1, s.renewals.Load())

			l.Unlock()
			synctest.Wait()
			time.Sleep(time.Second)
			synctest.Wait()
			require.Equal(t, before+1, s.renewals.Load())
		}
	})
}

func TestLongOperationDoesNotRenewAfterUnlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &shutdownLeaseStub{}
		l := &longOperation{lock: s, timeout: 3 * time.Second}
		for range 100 {
			require.NoError(t, l.Lock())
			synctest.Wait()
			time.Sleep(time.Second)
			l.Unlock()
			synctest.Wait()
		}
		require.Zero(t, s.renewalsAfterUnlock.Load())
	})
}
