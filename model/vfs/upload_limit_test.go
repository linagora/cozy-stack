package vfs_test

import (
	"strconv"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
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
	fill := func(t *testing.T, fs vfs.VFS, name string) int {
		n := 0
		for ; n < 10; n++ {
			_, err := vfs.NewUpload(fs, newUploadDoc(t, name+strconv.Itoa(n)+".bin", nil), nil, "", -1)
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

	t.Run("PerInstance", func(t *testing.T) {
		setUploadConfig(t, 3)
		fs := b.newFS()
		cleanAllUploads(t, fs)
		require.Equal(t, 3, fill(t, fs, "instance-"))
	})
}
