// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// GOMAXPROCS=10 go test

package lock

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// If you want to test harder the lock, you can set nb = 1000 but it is too
// slow for CI, and the lock package has very few commits in the last years.
var nb = 100

func redisTestURL() string {
	if raw := os.Getenv("COZY_TEST_REDIS_URL"); raw != "" {
		return raw
	}
	return "redis://localhost:6379/0"
}

func TestLock(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	flag.Parse()
	if testing.Short() {
		nb = 3
	}

	t.Run("MemLock", func(t *testing.T) {
		client := NewInMemory()

		db := prefixer.NewPrefixer(0, "cozy.local", "cozy.local")
		l := client.ReadWrite(db, "test-mem")
		hammerRW(t, l)
	})

	t.Run("RedisLock", func(t *testing.T) {
		opt, err := redis.ParseURL(redisTestURL())
		require.NoError(t, err)
		client := NewRedisLockGetter(redis.NewClient(opt))

		db := prefixer.NewPrefixer(0, "cozy.local", "cozy.local")
		l := client.ReadWrite(db, "test-redis")
		l.(*redisLock).timeout = time.Second
		l.(*redisLock).waitRetry = 100 * time.Millisecond

		require.NoError(t, l.RLock())
		require.NoError(t, l.RLock())
		l.RUnlock()
		require.Error(t, l.Lock())
		l.RUnlock()
		require.NoError(t, l.Lock())
		l.Unlock()

		hammerRW(t, l)

		done := make(chan bool)
		for i := 0; i < 10; i++ {
			go HammerMutex(l, done)
		}
		for i := 0; i < 10; i++ {
			<-done
		}

		other := client.ReadWrite(db, "test-redis")
		assert.NoError(t, l.Lock())
		assert.Error(t, other.Lock())

		l.Unlock()
	})

	t.Run("LongLock", func(t *testing.T) {
		if testing.Short() {
			return
		}

		opt, err := redis.ParseURL(redisTestURL())
		require.NoError(t, err)
		client := NewRedisLockGetter(redis.NewClient(opt))

		db := prefixer.NewPrefixer(0, "cozy.local", "cozy.local")
		long := client.LongOperation(db, "test-long")

		// Reduce the default timeout duration.
		long.(*longOperation).timeout = 50 * time.Millisecond

		l := client.ReadWrite(db, "test-long")
		l.(*redisLock).timeout = 200 * time.Millisecond
		l.(*redisLock).waitRetry = 10 * time.Millisecond

		// Take the lock and refresh it every 20ms without
		// losing the lock
		assert.NoError(t, long.Lock())

		// Try a second lock. It should fail after 200ms, and the long lock
		// will have been extended a few times in this span of time.
		err = l.Lock()
		assert.Error(t, err)
		assert.Equal(t, ErrTooManyRetries, err)
		long.Unlock()

		// Check that a long lock can be reused
		assert.NoError(t, long.Lock())
		long.Unlock()
	})
}

func reader(rwm ErrorRWLocker, iterations int, activity *int32, cdone chan bool) {
	for i := 0; i < iterations; i++ {
		err := rwm.RLock()
		if err != nil {
			panic(err)
		}
		n := atomic.AddInt32(activity, 1)
		if n < 1 || n >= 10000 {
			panic(fmt.Sprintf("wlock(%d)\n", n))
		}
		for i := 0; i < 100; i++ {
		}
		atomic.AddInt32(activity, -1)
		rwm.RUnlock()
	}
	cdone <- true
}

func writer(rwm ErrorRWLocker, iterations int, activity *int32, cdone chan bool) {
	for i := 0; i < iterations; i++ {
		err := rwm.Lock()
		if err != nil {
			panic(err)
		}
		n := atomic.AddInt32(activity, 10000)
		if n != 10000 {
			panic(fmt.Sprintf("wlock(%d)\n", n))
		}
		for i := 0; i < 100; i++ {
		}
		atomic.AddInt32(activity, -10000)
		rwm.Unlock()
	}
	cdone <- true
}

func HammerRWMutex(locker ErrorRWLocker, gomaxprocs, numReaders, iterations int) {
	runtime.GOMAXPROCS(gomaxprocs)
	// Number of active readers + 10000 * number of active writers.
	var activity int32
	cdone := make(chan bool)
	go writer(locker, iterations, &activity, cdone)
	var i int
	for i = 0; i < numReaders/2; i++ {
		go reader(locker, iterations, &activity, cdone)
	}
	go writer(locker, iterations, &activity, cdone)
	for ; i < numReaders; i++ {
		go reader(locker, iterations, &activity, cdone)
	}
	// Wait for the 2 writers and all readers to finish.
	for i := 0; i < 2+numReaders; i++ {
		<-cdone
	}
}

func HammerMutex(m ErrorLocker, cdone chan bool) {
	for i := 0; i < nb; i++ {
		err := m.Lock()
		if err != nil {
			panic(err)
		}
		m.Unlock()
	}
	cdone <- true
}

func hammerRW(t *testing.T, l ErrorRWLocker) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(-1))
	HammerRWMutex(l, 1, 1, nb)
	HammerRWMutex(l, 1, 3, nb)
	HammerRWMutex(l, 1, 10, nb)
	HammerRWMutex(l, 4, 1, nb)
	HammerRWMutex(l, 4, 3, nb)
	HammerRWMutex(l, 4, 10, nb)
	HammerRWMutex(l, 10, 1, nb)
	HammerRWMutex(l, 10, 3, nb)
	HammerRWMutex(l, 10, 10, nb)
	HammerRWMutex(l, 10, 5, nb)
}
