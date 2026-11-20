package vfs_test

import (
	"bytes"
	"crypto/md5"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResumableUploads(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			t.Run("InterruptedThenResumedAfterRestart", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(2*testPartSize+12345, 1)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, "resumed.bin", content), nil, "", -1)
				require.NoError(t, err)

				// The connection drops in the middle of the second part: the
				// bytes received so far are kept.
				cut := int64(testPartSize + 7777)
				_, err = vfs.AppendUpload(fs, u, &cutReader{bytes.NewReader(content), cut, errCut}, false)
				require.ErrorIs(t, err, errCut)
				assert.Equal(t, cut, u.Offset)

				// A new stack resumes from CouchDB.
				fs = b.newFS()
				u, err = vfs.ClaimUpload(fs, u.UploadID)
				require.NoError(t, err)
				assert.Equal(t, cut, u.Offset)

				// A second drop, in the middle of the tail.
				_, err = vfs.AppendUpload(fs, u, &cutReader{bytes.NewReader(content[cut:]), 1000, errCut}, false)
				require.ErrorIs(t, err, errCut)
				assert.Equal(t, cut+1000, u.Offset)

				doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content[cut+1000:]), true)
				require.NoError(t, err)
				assert.Equal(t, int64(len(content)), doc.ByteSize)
				sum := md5.Sum(content)
				assert.Equal(t, sum[:], doc.MD5Sum)
				assert.Equal(t, content, readContent(t, fs, doc))
				assert.Empty(t, tails(b.objects(u)))
				assert.False(t, b.pending(u))
				if b.name == "swift" {
					assert.True(t, strings.HasPrefix(doc.InternalID, "slo-"))
				}

				done, err := vfs.GetUpload(fs, u.UploadID)
				require.NoError(t, err)
				assert.True(t, done.Completed)
				assert.Equal(t, doc.ID(), done.File.ID())
				assert.Equal(t, int64(len(content)), done.Offset)
			})

			t.Run("SmallContentIsPlainFile", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(1<<20, 2)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, "small.bin", content), nil, "", -1)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content[:300000]), false)
				require.NoError(t, err)
				doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content[300000:]), true)
				require.NoError(t, err)
				assert.Equal(t, content, readContent(t, fs, doc))
				assert.False(t, strings.HasPrefix(doc.InternalID, "slo-"))
				assert.Empty(t, b.objects(u))
			})

			t.Run("WrongMD5", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(testPartSize+10, 3)
				doc := newUploadDoc(t, "wrong-md5.bin", []byte("something else"))
				u, err := vfs.NewUpload(fs, doc, nil, "", -1)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
				require.ErrorIs(t, err, vfs.ErrInvalidHash)
				_, err = vfs.GetUpload(fs, u.UploadID)
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)
				assert.Empty(t, b.objects(u))
				assert.False(t, b.pending(u))
				exists, err := fs.DirChildExists(consts.RootDirID, "wrong-md5.bin")
				require.NoError(t, err)
				assert.False(t, exists)
			})

			t.Run("LengthExceeded", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(testPartSize+100, 4)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, "length.bin", nil), nil, "", testPartSize+50)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), false)
				require.ErrorIs(t, err, vfs.ErrUploadLength)
				_, err = vfs.GetUpload(fs, u.UploadID)
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)
				assert.Empty(t, b.objects(u))
				assert.False(t, b.pending(u))
			})

			t.Run("Cancel", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(testPartSize+100, 5)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, "cancel.bin", nil), nil, "", -1)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), false)
				require.NoError(t, err)
				assert.NotEmpty(t, b.objects(u))
				require.NoError(t, vfs.CancelUpload(fs, u.UploadID))
				_, err = vfs.GetUpload(fs, u.UploadID)
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)
				_, err = vfs.ClaimUpload(fs, u.UploadID)
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)
				require.ErrorIs(t, vfs.CancelUpload(fs, u.UploadID), vfs.ErrUploadNotFound)
				assert.Empty(t, b.objects(u))
				assert.False(t, b.pending(u))
			})

			t.Run("ExpiredIsCleaned", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(testPartSize+100, 6)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, "expired.bin", nil), nil, "", -1)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), false)
				require.NoError(t, err)

				expireUpload(t, fs, u.UploadID)
				_, err = vfs.GetUpload(fs, u.UploadID)
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)
				// Appending to an expired upload writes nothing.
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), false)
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)

				removed, err := vfs.CleanUploads(fs)
				require.NoError(t, err)
				assert.Equal(t, 1, removed)
				assert.Empty(t, b.objects(u))
				assert.False(t, b.pending(u))
				err = couchdb.GetDoc(fs, consts.FilesUploads, u.UploadID, &vfs.Upload{})
				assert.True(t, couchdb.IsNotFoundError(err))
			})

			t.Run("TakenOver", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(testPartSize+100, 7)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, "taken.bin", nil), nil, "", -1)
				require.NoError(t, err)
				// A newer request resumes the upload: the older one, which was
				// not running, cannot write anymore.
				newer, err := vfs.ClaimUpload(fs, u.UploadID)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), false)
				require.ErrorIs(t, err, vfs.ErrUploadTakenOver)
				fresh, err := vfs.GetUpload(fs, u.UploadID)
				require.NoError(t, err)
				assert.Zero(t, fresh.Offset)

				doc, err := vfs.AppendUpload(fs, newer, bytes.NewReader(content), true)
				require.NoError(t, err)
				assert.Equal(t, content, readContent(t, fs, doc))
			})

			t.Run("CleanDuringAppend", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(2*testPartSize, 8)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, "race.bin", nil), nil, "", -1)
				require.NoError(t, err)

				// The client sends a part, then stalls.
				pr, pw := io.Pipe()
				done := make(chan error, 1)
				go func() {
					_, err := vfs.AppendUpload(fs, u, pr, true)
					_ = pr.CloseWithError(err)
					done <- err
				}()
				resume := make(chan struct{})
				go func() {
					if _, err := pw.Write(content[:testPartSize+10]); err != nil {
						return
					}
					<-resume
					if _, err := pw.Write(content[testPartSize+10:]); err != nil {
						return
					}
					_ = pw.Close()
				}()
				deadline := time.Now().Add(30 * time.Second)
				for {
					fresh, err := vfs.GetUpload(fs, u.UploadID)
					if err == nil && fresh.Offset == testPartSize {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("the append has stopped before the cleaning: %v", err)
					default:
					}
					require.True(t, time.Now().Before(deadline), "the first part is not written")
					time.Sleep(50 * time.Millisecond)
				}

				// The cleaning passes.
				expireUpload(t, fs, u.UploadID)
				removed, err := vfs.CleanUploads(fs)
				require.NoError(t, err)
				assert.Equal(t, 1, removed)

				close(resume)
				require.ErrorIs(t, <-done, vfs.ErrUploadNotFound)
				assert.Empty(t, b.objects(u))
				assert.False(t, b.pending(u))
				exists, err := fs.DirChildExists(consts.RootDirID, "race.bin")
				require.NoError(t, err)
				assert.False(t, exists)
			})

			t.Run("CompletedRecordIsCleaned", func(t *testing.T) {
				fs := b.newFS()
				content := randomContent(testPartSize+100, 9)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, "record.bin", content), nil, "", -1)
				require.NoError(t, err)
				doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
				require.NoError(t, err)

				expireUpload(t, fs, u.UploadID)
				removed, err := vfs.CleanUploads(fs)
				require.NoError(t, err)
				assert.Equal(t, 1, removed)
				_, err = vfs.GetUpload(fs, u.UploadID)
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)
				// The file stays.
				doc, err = fs.FileByID(doc.ID())
				require.NoError(t, err)
				assert.Equal(t, content, readContent(t, fs, doc))
			})

			t.Run("Overwrite", func(t *testing.T) {
				fs := b.newFS()
				old := newUploadDoc(t, "overwrite.bin", nil)
				old.ByteSize = 2
				f, err := fs.CreateFile(old, nil)
				require.NoError(t, err)
				_, err = f.Write([]byte("v1"))
				require.NoError(t, err)
				require.NoError(t, f.Close())
				old, err = fs.FileByID(old.ID())
				require.NoError(t, err)

				content := randomContent(testPartSize+100, 10)
				newdoc := newUploadDoc(t, old.DocName, content)
				newdoc.DirID = old.DirID
				u, err := vfs.NewUpload(fs, newdoc, old, old.Rev(), -1)
				require.NoError(t, err)
				doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
				require.NoError(t, err)
				assert.Equal(t, old.ID(), doc.ID())
				assert.Equal(t, content, readContent(t, fs, doc))
			})

			t.Run("OverwriteRenamedMeanwhile", func(t *testing.T) {
				fs := b.newFS()
				old := newUploadDoc(t, "before.bin", nil)
				old.ByteSize = 2
				f, err := fs.CreateFile(old, nil)
				require.NoError(t, err)
				_, err = f.Write([]byte("v1"))
				require.NoError(t, err)
				require.NoError(t, f.Close())
				old, err = fs.FileByID(old.ID())
				require.NoError(t, err)

				content := randomContent(testPartSize+100, 13)
				u, err := vfs.NewUpload(fs, newUploadDoc(t, old.DocName, content), old, "", -1)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content[:100]), false)
				require.NoError(t, err)

				// The file is renamed while its new content is uploaded.
				name := "after.bin"
				renamed, err := vfs.ModifyFileMetadata(fs, old, &vfs.DocPatch{Name: &name})
				require.NoError(t, err)

				doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content[100:]), true)
				require.NoError(t, err)
				assert.Equal(t, "after.bin", doc.DocName)
				assert.Equal(t, renamed.DirID, doc.DirID)
				assert.Equal(t, content, readContent(t, fs, doc))
			})

			t.Run("OverwriteChangedMeanwhile", func(t *testing.T) {
				fs := b.newFS()
				old := newUploadDoc(t, "changed.bin", nil)
				old.ByteSize = 2
				f, err := fs.CreateFile(old, nil)
				require.NoError(t, err)
				_, err = f.Write([]byte("v1"))
				require.NoError(t, err)
				require.NoError(t, f.Close())
				old, err = fs.FileByID(old.ID())
				require.NoError(t, err)

				content := randomContent(testPartSize+100, 11)
				newdoc := newUploadDoc(t, old.DocName, content)
				u, err := vfs.NewUpload(fs, newdoc, old, old.Rev(), -1)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content[:100]), false)
				require.NoError(t, err)

				// Another client renames the file in the meantime.
				changed := old.Clone().(*vfs.FileDoc)
				changed.Tags = []string{"changed"}
				require.NoError(t, fs.UpdateFileDoc(old, changed))

				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content[100:]), true)
				require.ErrorIs(t, err, vfs.ErrConflict)
				assert.Empty(t, b.objects(u))
			})

			t.Run("NameConflict", func(t *testing.T) {
				fs := b.newFS()
				doc := newUploadDoc(t, "conflict.bin", nil)
				doc.ByteSize = 2
				f, err := fs.CreateFile(doc, nil)
				require.NoError(t, err)
				_, err = f.Write([]byte("v1"))
				require.NoError(t, err)
				require.NoError(t, f.Close())
				_, err = vfs.NewUpload(fs, newUploadDoc(t, "conflict.bin", nil), nil, "", -1)
				require.ErrorIs(t, err, os.ErrExist)
			})

			t.Run("Quota", func(t *testing.T) {
				fs := b.newFS()
				usage, err := fs.DiskUsage()
				require.NoError(t, err)
				diskQuota = usage + testPartSize + 1000
				defer func() { diskQuota = 0 }()

				_, err = vfs.NewUpload(fs, newUploadDoc(t, "quota.bin", nil), nil, "", testPartSize+2000)
				require.ErrorIs(t, err, vfs.ErrFileTooBig)

				u, err := vfs.NewUpload(fs, newUploadDoc(t, "quota.bin", nil), nil, "", -1)
				require.NoError(t, err)
				_, err = vfs.AppendUpload(fs, u, bytes.NewReader(randomContent(testPartSize+2000, 12)), false)
				require.ErrorIs(t, err, vfs.ErrFileTooBig)
				_, err = vfs.GetUpload(fs, u.UploadID)
				require.ErrorIs(t, err, vfs.ErrUploadNotFound)
				assert.Empty(t, b.objects(u))
			})

			t.Run("TooManyUploads", func(t *testing.T) {
				setUploadConfig(t, 2)
				fs := b.newFS()
				_, _ = vfs.CleanUploads(fs)
				var ids []string
				for i := 0; ; i++ {
					u, err := vfs.NewUpload(fs, newUploadDoc(t, "many-"+string(rune('a'+i))+".bin", nil), nil, "", -1)
					if err != nil {
						require.ErrorIs(t, err, vfs.ErrTooManyUploads)
						break
					}
					ids = append(ids, u.UploadID)
					require.Less(t, i, 3)
				}
				for _, id := range ids {
					require.NoError(t, vfs.CancelUpload(fs, id))
				}
				_, err := vfs.NewUpload(fs, newUploadDoc(t, "many-z.bin", nil), nil, "", -1)
				require.NoError(t, err)
			})
		})
	}
}

// TestUploadSmallAppendsKeepThePartNumbers checks that the tails do not use
// the numbers of the parts: S3 accepts at most 10,000 parts, whatever the size
// of the requests.
func TestUploadSmallAppendsKeepThePartNumbers(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	for _, b := range uploadBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			fs := b.newFS()
			content := randomContent(2*testPartSize, 76)
			u, err := vfs.NewUpload(fs, newUploadDoc(t, "small-appends.bin", content), nil, "", -1)
			require.NoError(t, err)

			// Ten small requests, then one that fills a part.
			for i := range 10 {
				_, err := vfs.AppendUpload(fs, u, bytes.NewReader(content[i*1000:(i+1)*1000]), false)
				require.NoError(t, err)
			}
			_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content[10000:testPartSize+20000]), false)
			require.NoError(t, err)

			u, err = vfs.GetUpload(fs, u.UploadID)
			require.NoError(t, err)
			require.Len(t, u.Parts, 1)
			assert.Equal(t, 1, u.Parts[0].Number)

			doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content[testPartSize+20000:]), true)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(content, readContent(t, fs, doc)))
		})
	}
}
