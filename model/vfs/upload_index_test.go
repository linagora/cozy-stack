package vfs_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestActiveUploadsSkipTheCompletedOnes checks that the requests do not read
// the records of the completed uploads, which are kept for 7 days.
func TestActiveUploadsSkipTheCompletedOnes(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)
	b := aferoUploadBackend(t)
	fs := b.newFS()

	u, err := vfs.NewUpload(fs, newUploadDoc(t, "active.bin", nil), nil, "", -1)
	require.NoError(t, err)
	for i := 0; i < 30; i++ {
		done := &vfs.Upload{
			File:      newUploadDoc(t, "done.bin", nil),
			Completed: true,
			ExpiresAt: time.Now().Add(time.Hour),
		}
		require.NoError(t, couchdb.CreateDoc(fs, done))
	}

	active, err := vfs.ActiveUploads(fs)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, u.UploadID, active[0].UploadID)

	var rows []*vfs.Upload
	res, err := couchdb.FindDocsRaw(fs, consts.FilesUploads, &couchdb.FindRequest{
		UseIndex:       "by-completed",
		Selector:       mango.Equal("completed", false),
		ExecutionStats: true,
	}, &rows)
	require.NoError(t, err)
	assert.Equal(t, 1, res.ExecutionStats.TotalDocsExamined)
}

func uploadsDBExists(t *testing.T, fs vfs.VFS) bool {
	_, err := couchdb.DBStatus(fs, consts.FilesUploads)
	if couchdb.IsNoDatabaseError(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

// TestUploadsDatabaseCreatedAtTheFirstUpload checks that an instance gets the
// io.cozy.files.uploads database only with its first resumable upload.
func TestUploadsDatabaseCreatedAtTheFirstUpload(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)
	b := aferoUploadBackend(t)
	fs := b.newFS()
	require.False(t, uploadsDBExists(t, fs))

	t.Run("NothingCreatesItWithoutUpload", func(t *testing.T) {
		removed, err := vfs.CleanUploads(fs)
		require.NoError(t, err)
		assert.Equal(t, 0, removed)
		_, err = vfs.RemoveAllUploads(fs)
		require.NoError(t, err)
		_, err = vfs.GetUpload(fs, "unknown")
		assert.ErrorIs(t, err, vfs.ErrUploadNotFound)
		_, err = vfs.ClaimUpload(fs, "unknown")
		assert.ErrorIs(t, err, vfs.ErrUploadNotFound)
		assert.ErrorIs(t, vfs.CancelUpload(fs, "unknown"), vfs.ErrUploadNotFound)
		active, err := vfs.ActiveUploads(fs)
		require.NoError(t, err)
		assert.Empty(t, active)
		assert.False(t, uploadsDBExists(t, fs))
	})

	t.Run("ConcurrentFirstUploads", func(t *testing.T) {
		const n = 5
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				_, err := vfs.NewUpload(fs, newUploadDoc(t, fmt.Sprintf("first-%d.bin", i), nil), nil, "", -1)
				errs <- err
			}(i)
		}
		for i := 0; i < n; i++ {
			require.NoError(t, <-errs)
		}
		require.True(t, uploadsDBExists(t, fs))

		active, err := vfs.ActiveUploads(fs)
		require.NoError(t, err)
		assert.Len(t, active, n)
		var rows []*vfs.Upload
		res, err := couchdb.FindDocsRaw(fs, consts.FilesUploads, &couchdb.FindRequest{
			UseIndex:       "by-completed",
			Selector:       mango.Equal("completed", false),
			ExecutionStats: true,
		}, &rows)
		require.NoError(t, err, "the index must exist")
		assert.Equal(t, n, res.ExecutionStats.TotalDocsExamined)
	})
}

// TestUploadsDatabaseWithoutIndex checks that a database created without its
// index (a stack stopped between the two, or a failed definition) does not
// block the uploads of the instance.
func TestUploadsDatabaseWithoutIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)
	b := aferoUploadBackend(t)
	fs := b.newFS()
	require.NoError(t, couchdb.CreateDB(fs, consts.FilesUploads))

	u, err := vfs.NewUpload(fs, newUploadDoc(t, "no-index.bin", nil), nil, "", -1)
	require.NoError(t, err)
	active, err := vfs.ActiveUploads(fs)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, u.UploadID, active[0].UploadID)
}
