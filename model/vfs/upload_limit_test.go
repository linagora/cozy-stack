package vfs_test

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	b := aferoUploadBackend(t)

	cleanAllUploads := func(t *testing.T, fs vfs.VFS) {
		uploads, err := vfs.ActiveUploads(fs)
		require.NoError(t, err)
		for _, u := range uploads {
			require.NoError(t, vfs.CancelUpload(fs, u.UploadID))
		}
	}

	// fill creates uploads until the limit, and tells how many it has created.
	fill := func(t *testing.T, fs vfs.VFS, name string, member ...string) int {
		driveID, account := "", ""
		if len(member) > 0 {
			driveID, account = "drive", member[0]
		}
		n := 0
		for ; n < 10; n++ {
			_, err := vfs.NewDriveUpload(fs, newUploadDoc(t, name+strconv.Itoa(n)+".bin", nil), nil, "", -1, driveID, account)
			if err != nil {
				require.ErrorIs(t, err, vfs.ErrTooManyUploads)
				break
			}
		}
		t.Cleanup(func() { cleanAllUploads(t, fs) })
		return n
	}

	t.Run("PerAccount", func(t *testing.T) {
		setUploadConfig(t, 200)
		config.GetConfig().Fs.ResumableUploads.MaxPerAccount = 2
		fs := b.newFS()
		cleanAllUploads(t, fs)
		require.Equal(t, 2, fill(t, fs, "account-"))
	})

	t.Run("PerDriveMember", func(t *testing.T) {
		setUploadConfig(t, 5)
		config.GetConfig().Fs.ResumableUploads.MaxPerAccount = 2
		fs := b.newFS()
		cleanAllUploads(t, fs)
		require.Equal(t, 2, fill(t, fs, "betty-", "betty@example.net"))
		require.Equal(t, 2, fill(t, fs, "dave-", "dave@example.net"))
		// The owner has their own account, but the instance is at 5.
		require.Equal(t, 1, fill(t, fs, "owner-"))
	})

	t.Run("PerInstance", func(t *testing.T) {
		setUploadConfig(t, 3)
		fs := b.newFS()
		cleanAllUploads(t, fs)
		require.Equal(t, 3, fill(t, fs, "instance-"))
	})
}

// TestUploadLengthWithinTheSpaceLeft checks that a length announced after the
// creation of an upload must fit in the space left, like at its creation.
func TestUploadLengthWithinTheSpaceLeft(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)
	b := aferoUploadBackend(t)
	fs := b.newFS()
	usage, err := fs.DiskUsage()
	require.NoError(t, err)
	diskQuota = usage + 10*testPartSize
	defer func() { diskQuota = 0 }()

	t.Run("AnnouncedOnAnAppend", func(t *testing.T) {
		u, err := vfs.NewUpload(fs, newUploadDoc(t, "length-later.bin", nil), nil, "", -1)
		require.NoError(t, err)
		err = vfs.SetUploadLength(fs, u, 1<<62)
		require.ErrorIs(t, err, vfs.ErrFileTooBig)
		_, err = vfs.GetUpload(fs, u.UploadID)
		require.ErrorIs(t, err, vfs.ErrUploadNotFound)
	})

	t.Run("ReservationsAddedWithoutOverflow", func(t *testing.T) {
		for i := range 3 {
			u, err := vfs.NewUpload(fs, newUploadDoc(t, "huge-"+strconv.Itoa(i)+".bin", nil), nil, "", -1)
			require.NoError(t, err)
			t.Cleanup(func() { _ = vfs.CancelUpload(fs, u.UploadID) })
			u.Length = 1 << 62
			require.NoError(t, couchdb.UpdateDoc(fs, u))
		}
		u, err := vfs.NewUpload(fs, newUploadDoc(t, "after-huge.bin", nil), nil, "", -1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = vfs.CancelUpload(fs, u.UploadID) })
		limit, err := vfs.UploadSpaceLimit(fs, u)
		require.NoError(t, err)
		assert.Zero(t, limit)
	})
}

// TestUploadLimitsUnderConcurrency checks that the limits hold when the
// uploads are created at the same time, like the ones of a photo backup.
func TestUploadLimitsUnderConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 200)
	config.GetConfig().Fs.ResumableUploads.MaxPerAccount = 20
	b := aferoUploadBackend(t)
	fs := b.newFS()

	create := func(prefix string, n int) int {
		var accepted atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := vfs.NewUpload(fs, newUploadDoc(t, prefix+strconv.Itoa(i)+".bin", nil), nil, "", -1)
				if err == nil {
					accepted.Add(1)
				} else {
					assert.ErrorIs(t, err, vfs.ErrTooManyUploads)
				}
			}(i)
		}
		wg.Wait()
		return int(accepted.Load())
	}
	cancelAll := func() {
		uploads, err := vfs.ActiveUploads(fs)
		require.NoError(t, err)
		for _, u := range uploads {
			require.NoError(t, vfs.CancelUpload(fs, u.UploadID))
		}
	}

	t.Run("FromNineteen", func(t *testing.T) {
		defer cancelAll()
		require.Equal(t, 19, create("before-", 19))
		assert.Equal(t, 1, create("at-the-limit-", 5))
		active, err := vfs.ActiveUploads(fs)
		require.NoError(t, err)
		assert.Len(t, active, 20)
	})

	t.Run("FromZero", func(t *testing.T) {
		defer cancelAll()
		assert.Equal(t, 20, create("burst-", 30))
	})
}
