package lock

import (
	"errors"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
)

// Getter returns a lock on a resource matching the given `name`.
type Getter interface {
	// ReadWrite returns the read/write lock for the given name.
	// By convention, the name should be prefixed by the instance domain on which
	// it applies, then a slash and the package name (ie alice.example.net/vfs).
	ReadWrite(db prefixer.Prefixer, name string) ErrorRWLocker

	// LongOperation returns a lock suitable for long operations. It will refresh
	// the lock in redis to avoid its automatic expiration.
	LongOperation(db prefixer.Prefixer, name string) ErrorLocker
}

func New(client redis.UniversalClient) Getter {
	if client == nil {
		return NewInMemory()
	}

	return NewRedisLockGetter(client)
}

// An ErrorLocker is a locker which can fail (returns an error)
type ErrorLocker interface {
	Lock() error
	Unlock()
}

// ErrorRWLocker is the interface for a RWLock as inspired by RWMutex
type ErrorRWLocker interface {
	ErrorLocker
	RLock() error
	RUnlock()
}

type longOperationLocker interface {
	ErrorLocker
	Extend() error
}

// errLockLost means that an operation no longer owns its distributed lock.
var errLockLost = errors.New("lock ownership lost")

type longOperation struct {
	lock    longOperationLocker
	mu      sync.Mutex
	done    chan struct{}
	timeout time.Duration
}

func (l *longOperation) Lock() error {
	if err := l.lock.Lock(); err != nil {
		return err
	}
	l.mu.Lock()
	done := make(chan struct{})
	l.done = done
	l.mu.Unlock()
	go func() {
		tick := time.NewTicker(l.timeout / 3)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				// A lease that cannot be renewed is not going to start
				// renewing again, so the goroutine stops rather than logging
				// the same failure every tick until Unlock.
				if err := l.lock.Extend(); err != nil {
					logger.WithNamespace("lock").
						Warnf("cannot extend a long operation lease: %s", err)
					return
				}
			}
		}
	}()
	return nil
}

func (l *longOperation) Unlock() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done != nil {
		close(l.done)
		l.done = nil
	}
	l.lock.Unlock()
}
