package lock

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type leaseStub struct {
	mu       sync.Mutex
	err      error
	renewals int
}

func (s *leaseStub) Lock() error { return nil }
func (s *leaseStub) Unlock()     {}
func (s *leaseStub) Extend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewals++
	return s.err
}

func (s *leaseStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewals
}

// The renewer used to hold the mutex across the whole loop and stop on a nil
// ticker, so a failing lease was retried forever and Unlock raced with it.
func TestLongOperationRenewsUntilItCannot(t *testing.T) {
	s := &leaseStub{}
	l := &longOperation{lock: s, timeout: 30 * time.Millisecond}
	require.NoError(t, l.Lock())
	require.Eventually(t, func() bool { return s.count() > 1 }, time.Second, time.Millisecond)

	s.mu.Lock()
	s.err = errors.New("redis unavailable")
	s.mu.Unlock()
	require.Eventually(t, func() bool {
		before := s.count()
		time.Sleep(50 * time.Millisecond)
		return s.count() == before
	}, 2*time.Second, 10*time.Millisecond, "a lease that cannot be renewed stops being renewed")

	l.Unlock()
}

func TestLongOperationStopsRenewingOnUnlock(t *testing.T) {
	s := &leaseStub{}
	l := &longOperation{lock: s, timeout: 30 * time.Millisecond}
	require.NoError(t, l.Lock())
	require.Eventually(t, func() bool { return s.count() > 0 }, time.Second, time.Millisecond)

	l.Unlock()
	after := s.count()
	time.Sleep(60 * time.Millisecond)
	require.Equal(t, after, s.count())
}
