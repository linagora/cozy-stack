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

// TestUploadCutThenResumedAtOnce checks that the offset answered to a client
// that resumes just after a cut is durable: the new request waits for the cut
// one to save the bytes it has received, or stops it after a while, and an
// append from this offset is accepted (draft-ietf-httpbis-resumable-upload
// §10).
func TestUploadCutThenResumedAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		for _, cut := range []int64{1000, testPartSize + 1000} {
			for _, stuck := range []bool{false, true} {
				name := fmt.Sprintf("%s/%d/saved", b.name, cut)
				if stuck {
					name = fmt.Sprintf("%s/%d/stuck", b.name, cut)
				}
				t.Run(name, func(t *testing.T) {
					defer vfs.SetClaimWait(time.Second)()
					fs := b.newFS()
					content := randomContent(2*testPartSize+500, 74)
					u, err := vfs.NewUpload(fs, newUploadDoc(t, fmt.Sprintf("cut-%d-%t.bin", cut, stuck), content), nil, "", -1)
					require.NoError(t, err)

					// The request is cut after cut bytes; the stack sees the
					// end of the body, but the HEAD of the client arrives
					// before it saves the bytes.
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

					claimed := make(chan *vfs.Upload, 1)
					go func() {
						head, err := vfs.ClaimUpload(fs, u.UploadID)
						assert.NoError(t, err)
						claimed <- head
					}()
					var head *vfs.Upload
					want := cut
					if stuck {
						// The cut request stays stuck longer than the wait: it
						// cannot write anymore, its last bytes are lost.
						head = <-claimed
						close(release)
						require.ErrorIs(t, <-done, vfs.ErrUploadTakenOver)
						want = cut / testPartSize * testPartSize
					} else {
						// The HEAD has taken the upload, and waits.
						require.Eventually(t, func() bool {
							fresh, err := vfs.GetUpload(fs, u.UploadID)
							return err == nil && fresh.Previous != ""
						}, 5*time.Second, 10*time.Millisecond)
						close(release)
						head = <-claimed
						require.Error(t, <-done)
					}
					require.NotNil(t, head)
					assert.Equal(t, want, head.Offset)

					// The answer of the HEAD is not outdated: the client
					// resumes from it.
					fresh, err := vfs.GetUpload(fs, u.UploadID)
					require.NoError(t, err)
					assert.Equal(t, head.Offset, fresh.Offset)
					assert.Empty(t, fresh.Previous)
					doc, err := vfs.AppendUpload(fs, head, bytes.NewReader(content[head.Offset:]), true)
					require.NoError(t, err)
					assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))
					sum := md5.Sum(content)
					assert.Equal(t, sum[:], doc.MD5Sum)
					assert.Empty(t, tails(b.objects(head)))
				})
			}
		}
	}
}

// TestUploadClaimsShareTheirWait checks that the requests that wait for an
// older one, like several HEAD of a client, read the upload a few times only.
func TestUploadClaimsShareTheirWait(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)
	defer vfs.SetClaimWait(time.Second)()

	b := aferoUploadBackend(t)
	fs := b.newFS()
	content := randomContent(testPartSize, 77)
	u, err := vfs.NewUpload(fs, newUploadDoc(t, "shared-wait.bin", content), nil, "", -1)
	require.NoError(t, err)

	// The older request stays stuck with its last bytes.
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
		_, err := vfs.AppendUpload(fs, u, &cutReader{bytes.NewReader(content), 1000, errCut}, false)
		done <- err
	}()
	<-reached

	reads, restoreReads := vfs.CountClaimReads()
	defer restoreReads()
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			head, err := vfs.ClaimUpload(fs, u.UploadID)
			assert.NoError(t, err)
			assert.Zero(t, head.Offset)
		}()
	}
	wg.Wait()
	close(release)
	require.ErrorIs(t, <-done, vfs.ErrUploadTakenOver)
	assert.LessOrEqual(t, reads.Load(), int64(20), "%d reads for %d requests", reads.Load(), n)
}
