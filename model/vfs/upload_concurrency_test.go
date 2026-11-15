package vfs_test

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUploadsWriteInParallel checks that the parts of several uploads of an
// instance are written at the same time: each write waits until all of them
// are in progress.
func TestUploadsWriteInParallel(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	const n = 9
	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			var writing, overlap atomic.Int32
			all := make(chan struct{})
			var once sync.Once
			restore := vfs.SetUploadStepHook(func(step string) error {
				if step != "write" {
					return nil
				}
				now := writing.Add(1)
				if now > overlap.Load() {
					overlap.Store(now)
				}
				if now == n {
					once.Do(func() { close(all) })
				}
				select {
				case <-all:
				case <-time.After(20 * time.Second):
				}
				return nil
			})
			defer restore()

			contents := make([][]byte, n)
			docs := make([]*vfs.FileDoc, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				contents[i] = randomContent(testPartSize+10, int64(60+i))
				u, err := vfs.NewUpload(fs, newUploadDoc(t, fmt.Sprintf("parallel-%d.bin", i), contents[i]), nil, "", -1)
				require.NoError(t, err)
				wg.Add(1)
				go func(i int, u *vfs.Upload) {
					defer wg.Done()
					docs[i], errs[i] = vfs.AppendUpload(fs, u, bytes.NewReader(contents[i]), true)
				}(i, u)
			}
			wg.Wait()
			assert.EqualValues(t, n, overlap.Load(), "the parts must be written at the same time")
			for i := 0; i < n; i++ {
				require.NoError(t, errs[i])
				assert.True(t, bytes.Equal(contents[i], readContent(t, fs, docs[i])))
			}
		})
	}
}

// TestUploadResumedDuringAWrite checks that the write of a request that has
// lost the upload does not count, and is removed.
func TestUploadResumedDuringAWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			content := randomContent(2*testPartSize+10, 70)
			older, err := vfs.NewUpload(fs, newUploadDoc(t, "resumed-during-write.bin", content), nil, "", -1)
			require.NoError(t, err)

			// The older request stops in the middle of its first write.
			paused := make(chan struct{})
			resume := make(chan struct{})
			var first atomic.Bool
			restore := vfs.SetUploadStepHook(func(step string) error {
				if step == "written" && first.CompareAndSwap(false, true) {
					close(paused)
					<-resume
				}
				return nil
			})
			defer restore()
			done := make(chan error, 1)
			go func() {
				_, err := vfs.AppendUpload(fs, older, bytes.NewReader(content[:testPartSize+10]), false)
				done <- err
			}()
			<-paused

			// A newer request resumes the upload and finishes it.
			newer, err := vfs.ClaimUpload(fs, older.UploadID)
			require.NoError(t, err)
			doc, err := vfs.AppendUpload(fs, newer, bytes.NewReader(content), true)
			require.NoError(t, err)

			close(resume)
			require.ErrorIs(t, <-done, vfs.ErrUploadTakenOver)
			assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))
			assert.Empty(t, tails(b.objects(newer)))
		})
	}
}

// TestUploadOrphanPartsRemoved checks that a part written by a request that
// stops before counting it is removed at the end of the upload.
func TestUploadOrphanPartsRemoved(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			content := randomContent(2*testPartSize+10, 71)
			u, err := vfs.NewUpload(fs, newUploadDoc(t, "orphan.bin", content), nil, "", -1)
			require.NoError(t, err)

			restore := vfs.StopUploadAfter("written", errStopped)
			_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content[:testPartSize+10]), false)
			restore()
			require.ErrorIs(t, err, errStopped)
			orphans := b.objects(u)

			u, err = vfs.ClaimUpload(fs, u.UploadID)
			require.NoError(t, err)
			assert.Equal(t, int64(0), u.Offset)
			doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))
			left := b.objects(u)
			for _, o := range orphans {
				assert.NotContains(t, left, o, "the orphan part must be removed")
			}
		})
	}
}
