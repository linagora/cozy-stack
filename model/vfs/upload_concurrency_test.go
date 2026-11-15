package vfs_test

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
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
			// No write leaves before the n-th has started, unless it times out.
			var writing atomic.Int32
			var timedOut atomic.Bool
			all := make(chan struct{})
			var once sync.Once
			restore := vfs.SetUploadStepHook(func(step string) error {
				if step != "write" {
					return nil
				}
				if writing.Add(1) == n {
					once.Do(func() { close(all) })
				}
				select {
				case <-all:
				case <-time.After(20 * time.Second):
					timedOut.Store(true)
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
			assert.False(t, timedOut.Load(), "the parts must be written at the same time")
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

// TestUploadFinishedDuringAStorageMigration checks that a storage migration,
// which holds the lock of the VFS and removes the uploads, does not wait for
// an upload that is finishing, and that this upload is not indexed once its
// content has been removed.
func TestUploadFinishedDuringAStorageMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			content := randomContent(testPartSize+100, 72)
			name := "during-migration.bin"
			u, err := vfs.NewUpload(fs, newUploadDoc(t, name, content), nil, "", -1)
			require.NoError(t, err)

			// The end of the upload stops just before the indexing, which takes
			// the lock of the VFS.
			reached := make(chan struct{})
			release := make(chan struct{})
			restore := vfs.SetUploadStepHook(func(step string) error {
				if step == "commit" {
					close(reached)
					<-release
				}
				return nil
			})
			defer restore()
			finished := make(chan error, 1)
			go func() {
				_, err := vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
				finished <- err
			}()
			<-reached

			// What the storage migration does: the lock of the VFS, then the
			// removal of the uploads.
			migration := config.Lock().LongOperation(fs, "vfs")
			require.NoError(t, migration.Lock())
			source := b.newFSWithLock("vfs-migration-source")
			removed := make(chan error, 1)
			go func() {
				_, err := vfs.RemoveAllUploads(source)
				removed <- err
			}()
			select {
			case err := <-removed:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("the removal of the uploads waits for the upload")
			}
			close(release)
			migration.Unlock()

			select {
			case err := <-finished:
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)
			case <-time.After(30 * time.Second):
				t.Fatal("the upload does not finish")
			}
			exists, err := fs.DirChildExists(consts.RootDirID, name)
			require.NoError(t, err)
			assert.False(t, exists, "a removed upload must not be indexed")
		})
	}
}

// TestParallelUploadsShareTheQuota checks that two uploads of unknown length,
// sent at the same time, do not store more than the available space together.
func TestParallelUploadsShareTheQuota(t *testing.T) {
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
	diskQuota = usage + 2*testPartSize
	defer func() { diskQuota = 0 }()
	available := int64(2 * testPartSize)

	u1, err := vfs.NewUpload(fs, newUploadDoc(t, "quota-1.bin", nil), nil, "", -1)
	require.NoError(t, err)
	u2, err := vfs.NewUpload(fs, newUploadDoc(t, "quota-2.bin", nil), nil, "", -1)
	require.NoError(t, err)

	// Each request sends the whole available space, in two parts.
	content := randomContent(int(available), 73)
	send := func(u *vfs.Upload) chan error {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			_, err := vfs.AppendUpload(fs, u, pr, false)
			_ = pr.CloseWithError(err)
			done <- err
		}()
		go func() {
			// Both requests have started before the content arrives.
			time.Sleep(300 * time.Millisecond)
			_, _ = pw.Write(content)
			_ = pw.Close()
		}()
		return done
	}
	d1, d2 := send(u1), send(u2)
	err1, err2 := <-d1, <-d2

	var stored int64
	for _, id := range []string{u1.UploadID, u2.UploadID} {
		if u, err := vfs.GetUpload(fs, id); err == nil {
			stored += u.Offset
		}
	}
	assert.LessOrEqual(t, stored, available, "the uploads must not store more than the available space")
	refused := 0
	for _, err := range []error{err1, err2} {
		if err != nil {
			require.ErrorIs(t, err, vfs.ErrFileTooBig)
			refused++
		}
	}
	assert.GreaterOrEqual(t, refused, 1)
}

// TestUploadCancelledDuringItsCommit checks that a cancel that arrives while
// the file is being indexed waits for the lock of the VFS, and keeps the
// content of the indexed file.
func TestUploadCancelledDuringItsCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			content := randomContent(testPartSize+10, 75)
			u, err := vfs.NewUpload(fs, newUploadDoc(t, "cancelled-during-commit.bin", content), nil, "", -1)
			require.NoError(t, err)

			cancelled := make(chan error, 1)
			var first atomic.Bool
			restore := vfs.SetUploadStepHook(func(step string) error {
				if step == "locked" && first.CompareAndSwap(false, true) {
					go func() { cancelled <- vfs.CancelUpload(b.newFS(), u.UploadID) }()
					// Leave time to the cancel to remove the content, if it
					// does not wait for the lock.
					time.Sleep(300 * time.Millisecond)
				}
				return nil
			})
			defer restore()
			_, _ = vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
			require.NoError(t, <-cancelled)

			doc, err := fs.FileByID(u.File.ID())
			require.NoError(t, err)
			assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))
		})
	}
}

// TestUploadCutThenResumedAtOnce checks that the bytes of a cut request are
// kept when the client resumes the upload before the stack has saved them:
// the HEAD of the client takes the upload, but the cut request still saves
// its last write, contiguous to the offset.
func TestUploadCutThenResumedAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		for _, cut := range []int64{1000, testPartSize + 1000} {
			t.Run(fmt.Sprintf("%s/%d", b.name, cut), func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(2*testPartSize+500, 74)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, fmt.Sprintf("cut-%d.bin", cut), content), nil, "", -1)
				require.NoError(t, err)

				// The request is cut after cut bytes; the stack sees the end of
				// the body, but the HEAD of the client arrives before it saves
				// the bytes.
				reached := make(chan struct{})
				release := make(chan struct{})
				var first atomic.Bool
				restore := vfs.SetUploadStepHook(func(step string) error {
					if step == "received" && first.CompareAndSwap(false, true) {
						close(reached)
						<-release
					}
					return nil
				})
				defer restore()
				done := make(chan error, 1)
				go func() {
					_, err := vfs.AppendUpload(fs, u, &cutReader{bytes.NewReader(content), cut, errCut}, false)
					done <- err
				}()
				<-reached
				head, err := vfs.ClaimUpload(fs, u.UploadID)
				require.NoError(t, err)
				assert.LessOrEqual(t, head.Offset, cut)
				close(release)
				require.Error(t, <-done)

				// The bytes of the cut request are kept.
				fresh, err := vfs.GetUpload(fs, u.UploadID)
				require.NoError(t, err)
				assert.Equal(t, cut, fresh.Offset)

				// The client resumes from the offset of its HEAD: the stack
				// answers that the offset has moved, and the client resumes
				// from the new one.
				if head.Offset != fresh.Offset {
					_, err = vfs.AppendUpload(fs, head, bytes.NewReader(content[head.Offset:]), true)
					require.ErrorIs(t, err, vfs.ErrUploadTakenOver)
				}
				resumed, err := vfs.ClaimUpload(fs, u.UploadID)
				require.NoError(t, err)
				require.Equal(t, cut, resumed.Offset)
				doc, err := vfs.AppendUpload(fs, resumed, bytes.NewReader(content[cut:]), true)
				require.NoError(t, err)
				assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))
				sum := md5.Sum(content)
				assert.Equal(t, sum[:], doc.MD5Sum)
			})
		}
	}
}
