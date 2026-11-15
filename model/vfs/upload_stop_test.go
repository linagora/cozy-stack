package vfs_test

import (
	"bytes"
	"crypto/md5"
	"errors"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStopped = errors.New("the stack has stopped")

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
