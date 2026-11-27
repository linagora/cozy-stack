package vfs_test

import (
	"bytes"
	"crypto/md5"
	"errors"
	"io"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStopped = errors.New("the stack has stopped")

// TestResumableUploadCommitFailsOnce checks that an upload stays resumable
// when the indexing fails for a passing reason, like CouchDB being briefly
// unavailable.
func TestResumableUploadCommitFailsOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			content := randomContent(testPartSize+5000, 51)
			u, err := vfs.NewUpload(fs, newUploadDoc(t, "commit-fails.bin", content), nil, "", -1)
			require.NoError(t, err)

			restore := vfs.StopUploadAfter("commit", errors.New("CouchDB is unavailable"))
			_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
			restore()
			require.Error(t, err)

			u, err = vfs.ClaimUpload(fs, u.UploadID)
			require.NoError(t, err, "the upload must stay resumable")
			assert.Equal(t, int64(len(content)), u.Offset)

			doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(nil), true)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))
		})
	}
}

// TestResumableUploadStoppedAtTheEnd stops the stack after each step of the
// end of an upload, and checks that the client finishes it with an empty
// request, without any byte written twice.
func TestResumableUploadStoppedAtTheEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		for _, step := range []string{"finalizing", "assembled", "committed"} {
			t.Run(b.name+"/"+step, func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(testPartSize+5000, 50)
				name := "stopped-" + step + ".bin"
				u, err := vfs.NewUpload(fs, newUploadDoc(t, name, content), nil, "", -1)
				require.NoError(t, err)
				cut := int64(testPartSize + 1000)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content[:cut]), false)
				require.NoError(t, err)

				restore := vfs.StopUploadAfter(step, errStopped)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content[cut:]), true)
				restore()
				require.ErrorIs(t, err, errStopped)

				// A new stack: HEAD gives the final size, so the client sends
				// an empty request to finish.
				fs = b.newFS()
				u, err = vfs.ClaimUpload(fs, u.UploadID)
				require.NoError(t, err)
				assert.Equal(t, int64(len(content)), u.Offset)
				assert.False(t, u.Completed)

				doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(nil), true)
				require.NoError(t, err)
				got := readContent(t, fs, doc)
				assert.Equal(t, len(content), len(got))
				assert.True(t, bytes.Equal(content, got), "the content differs")
				sum := md5.Sum(content)
				assert.Equal(t, sum[:], doc.MD5Sum)
				assert.Empty(t, tails(b.objects(u)))

				exists, err := fs.DirChildExists(consts.RootDirID, name)
				require.NoError(t, err)
				assert.True(t, exists)
				done, err := vfs.GetUpload(fs, u.UploadID)
				require.NoError(t, err)
				assert.True(t, done.Completed)
			})
		}
	}
}

// TestResumableUploadCleanedAfterANewVersion checks that the cleaning of an
// upload, stopped after its indexing, does not remove its content once it
// has become a version of the file.
func TestResumableUploadCleanedAfterANewVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)
	config.GetConfig().Fs.Versioning = config.FsVersioning{MaxNumberToKeep: 5}

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			content := randomContent(testPartSize+5000, 52)
			u, err := vfs.NewUpload(fs, newUploadDoc(t, "versioned.bin", content), nil, "", -1)
			require.NoError(t, err)
			restore := vfs.StopUploadAfter("committed", errStopped)
			_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
			restore()
			require.ErrorIs(t, err, errStopped)

			// The file gets a new content: the uploaded one becomes a version.
			doc, err := fs.FileByID(u.File.ID())
			require.NoError(t, err)
			newdoc := newUploadDoc(t, doc.DocName, nil)
			newdoc.ByteSize = 2
			f, err := fs.CreateFile(newdoc, doc)
			require.NoError(t, err)
			_, err = f.Write([]byte("v2"))
			require.NoError(t, err)
			require.NoError(t, f.Close())

			expireUpload(t, fs, u.UploadID)
			_, err = vfs.CleanUploads(fs)
			require.NoError(t, err)

			doc, err = fs.FileByID(doc.ID())
			require.NoError(t, err)
			versions, err := vfs.VersionsFor(fs, doc.ID())
			require.NoError(t, err)
			require.Len(t, versions, 1)
			old, err := fs.OpenFileVersion(doc, versions[0])
			require.NoError(t, err, "the version must still be readable")
			got, err := io.ReadAll(old)
			require.NoError(t, err)
			require.NoError(t, old.Close())
			assert.True(t, bytes.Equal(content, got))
		})
	}
}

// failingReads is a VFS whose reads of the index fail, like a lock of the VFS
// that times out while another request holds it.
type failingReads struct {
	vfs.VFS
	vfs.UploadStorage
}

var errReadTimeout = errors.New("timeout while waiting for the lock")

func (failingReads) FileByID(string) (*vfs.FileDoc, error) { return nil, errReadTimeout }

// TestUploadRemovalKeepsAnIndexedFile checks that the removal of an upload
// keeps its content when it cannot tell if the file has been indexed.
func TestUploadRemovalKeepsAnIndexedFile(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			content := randomContent(testPartSize+5000, 52)
			u, err := vfs.NewUpload(fs, newUploadDoc(t, "removal-keeps.bin", content), nil, "", -1)
			require.NoError(t, err)

			// The file is indexed, but the upload is not marked as completed.
			restore := vfs.StopUploadAfter("committed", errStopped)
			_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
			restore()
			require.ErrorIs(t, err, errStopped)

			err = vfs.CancelUpload(failingReads{fs, fs.(vfs.UploadStorage)}, u.UploadID)
			require.ErrorIs(t, err, errReadTimeout)
			doc, err := fs.FileByID(u.File.ID())
			require.NoError(t, err)
			assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))

			// The cleaning finishes the removal, and keeps the file.
			_, err = vfs.CleanUploads(fs)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))
		})
	}
}
