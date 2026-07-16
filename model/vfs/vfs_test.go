package vfs_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsafero"
	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/model/vfs/vfsswift"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/ncw/swift/v2/swifttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

var mutex lock.ErrorRWLocker
var diskQuota int64

type diskImpl struct{}

type H map[string]H

type contexter struct {
	cluster int
	domain  string
	prefix  string
	context string
}

func TestVfs(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	aferoFS := makeAferoFS(t)
	swiftFS := makeSwiftFS(t)
	s3FS := makeS3FS(t)

	var tests = []struct {
		name string
		fs   vfs.VFS
	}{
		{"afero", aferoFS},
		{"swift", swiftFS},
		{"s3", s3FS},
	}

	for _, tt := range tests {
		fs := tt.fs

		t.Run(tt.name, func(t *testing.T) {
			t.Run("DiskUsageIsInitiallyZero", func(t *testing.T) {
				used, err := fs.DiskUsage()
				assert.NoError(t, err)
				assert.Equal(t, int64(0), used)
			})

			t.Run("GetFileDocFromPathAtRoot", func(t *testing.T) {
				doc, err := vfs.NewFileDoc("toto", "", -1, nil, "foo/bar", "foo", time.Now(), false, false, false, []string{})
				assert.NoError(t, err)

				body := bytes.NewReader([]byte("hello !"))

				file, err := fs.CreateFile(doc, nil)
				assert.NoError(t, err)

				n, err := io.Copy(file, body)
				assert.NoError(t, err)
				assert.Equal(t, len("hello !"), int(n))

				err = file.Close()
				assert.NoError(t, err)

				_, err = fs.FileByPath("/toto")
				assert.NoError(t, err)

				_, err = fs.FileByPath("/noooo")
				assert.Error(t, err)
			})

			t.Run("Remove", func(t *testing.T) {
				err := vfs.Remove(fs, "foo/bar", fs.EnsureErased)
				assert.Error(t, err)
				assert.Equal(t, vfs.ErrNonAbsolutePath, err)

				err = vfs.Remove(fs, "/foo", fs.EnsureErased)
				assert.Error(t, err)
				assert.Equal(t, "file does not exist", err.Error())

				_, err = vfs.Mkdir(fs, "/removeme", nil)
				if !assert.NoError(t, err) {
					err = vfs.Remove(fs, "/removeme", fs.EnsureErased)
					assert.NoError(t, err)
				}
			})

			t.Run("RemoveAll", func(t *testing.T) {
				origtree := H{
					"removemeall/": H{
						"dirchild1/": H{
							"food/": H{},
							"bard/": H{},
						},
						"dirchild2/": H{
							"foof": nil,
							"barf": nil,
						},
						"dirchild3/": H{},
						"filechild1": nil,
					},
				}
				_ = createTree(t, fs, origtree, consts.RootDirID)

				err := vfs.RemoveAll(fs, "/removemeall", fs.EnsureErased)
				require.NoError(t, err)

				_, err = fs.DirByPath("/removemeall/dirchild1")
				assert.Error(t, err)
				_, err = fs.DirByPath("/removemeall")
				assert.Error(t, err)
			})

			t.Run("DiskUsage", func(t *testing.T) {
				used, err := fs.DiskUsage()
				assert.NoError(t, err)
				assert.Equal(t, len("hello !"), int(used))
			})

			t.Run("GetFileDocFromPath", func(t *testing.T) {
				dir, _ := vfs.NewDirDoc(fs, "container", "", nil)
				err := fs.CreateDir(dir)
				assert.NoError(t, err)

				doc, err := vfs.NewFileDoc("toto", dir.ID(), -1, nil, "foo/bar", "foo", time.Now(), false, false, false, []string{})
				assert.NoError(t, err)

				body := bytes.NewReader([]byte("hello !"))

				file, err := fs.CreateFile(doc, nil)
				assert.NoError(t, err)

				n, err := io.Copy(file, body)
				assert.NoError(t, err)
				assert.Equal(t, len("hello !"), int(n))

				err = file.Close()
				assert.NoError(t, err)

				_, err = fs.FileByPath("/container/toto")
				assert.NoError(t, err)

				_, err = fs.FileByPath("/container/noooo")
				assert.Error(t, err)
			})

			t.Run("CreateGetAndModifyFile", func(t *testing.T) {
				origtree := H{
					"createandget1/": H{
						"dirchild1/": H{
							"food/": H{},
							"bard/": H{},
						},
						"dirchild2/": H{
							"foof": nil,
							"barf": nil,
						},
						"dirchild3/": H{},
						"filechild1": nil,
					},
				}

				olddoc := createTree(t, fs, origtree, consts.RootDirID)

				newname := "createandget2"
				_, err := vfs.ModifyDirMetadata(fs, olddoc, &vfs.DocPatch{
					Name: &newname,
				})
				require.NoError(t, err)

				tree, err := fetchTree(fs, "/createandget2")
				require.NoError(t, err)

				assert.EqualValues(t, origtree["createandget1/"], tree["createandget2/"], "should have same tree")

				fileBefore, err := fs.FileByPath("/createandget2/dirchild2/foof")
				require.NoError(t, err)

				newfilename := "foof.jpg"
				_, err = vfs.ModifyFileMetadata(fs, fileBefore, &vfs.DocPatch{
					Name: &newfilename,
				})
				require.NoError(t, err)

				fileAfter, err := fs.FileByPath("/createandget2/dirchild2/foof.jpg")
				require.NoError(t, err)

				assert.Equal(t, "files", fileBefore.Class)
				assert.Equal(t, "application/octet-stream", fileBefore.Mime)
				assert.Equal(t, "image", fileAfter.Class)
				assert.Equal(t, "image/jpeg", fileAfter.Mime)
			})

			t.Run("UpdateDir", func(t *testing.T) {
				origtree := H{
					"update1/": H{
						"dirchild1/": H{
							"food/": H{},
							"bard/": H{},
						},
						"dirchild2/": H{
							"foof": nil,
							"barf": nil,
						},
						"dirchild3/": H{},
						"filechild1": nil,
					},
				}

				doc1 := createTree(t, fs, origtree, consts.RootDirID)

				newname := "update2"
				_, err := vfs.ModifyDirMetadata(fs, doc1, &vfs.DocPatch{
					Name: &newname,
				})
				require.NoError(t, err)

				tree, err := fetchTree(fs, "/update2")
				require.NoError(t, err)

				if !assert.EqualValues(t, origtree["update1/"], tree["update2/"], "should have same tree") {
					return
				}

				dirchild2, err := fs.DirByPath("/update2/dirchild2")
				require.NoError(t, err)

				dirchild3, err := fs.DirByPath("/update2/dirchild3")
				require.NoError(t, err)

				newfolid := dirchild2.ID()
				_, err = vfs.ModifyDirMetadata(fs, dirchild3, &vfs.DocPatch{
					DirID: &newfolid,
				})
				require.NoError(t, err)

				tree, err = fetchTree(fs, "/update2")
				require.NoError(t, err)

				assert.EqualValues(t, H{
					"update2/": H{
						"dirchild1/": H{
							"bard/": H{},
							"food/": H{},
						},
						"filechild1": nil,
						"dirchild2/": H{
							"barf":       nil,
							"foof":       nil,
							"dirchild3/": H{},
						},
					},
				}, tree)
			})

			t.Run("EncodingOfDirName", func(t *testing.T) {
				base := "encoding-dir"
				nfc := "chaîne"
				nfd := "chaîne"

				origtree := H{base + "/": H{
					nfc: H{},
					nfd: H{},
				}}
				_ = createTree(t, fs, origtree, consts.RootDirID)

				f1, err := fs.FileByPath("/" + base + "/" + nfc)
				require.NoError(t, err)
				assert.Equal(t, nfc, f1.DocName)

				f2, err := fs.FileByPath("/" + base + "/" + nfd)
				require.NoError(t, err)
				assert.Equal(t, nfd, f2.DocName)

				assert.NotEqual(t, f1.DocID, f2.DocID)
			})

			t.Run("ChangeEncodingOfDirName", func(t *testing.T) {
				nfc := "dir-nfc-to-nfd-é"
				nfd := "dir-nfc-to-nfd-é"

				origtree := H{nfc + "/": H{
					"dirchild1/": H{
						"food/": H{},
						"bard/": H{},
					},
					"dirchild2/": H{},
					"filechild1": nil,
				}}
				doc := createTree(t, fs, origtree, consts.RootDirID)

				newname := nfd
				doc, err := vfs.ModifyDirMetadata(fs, doc, &vfs.DocPatch{
					Name: &newname,
				})
				require.NoError(t, err)
				d, err := fs.DirByPath("/" + newname)
				require.NoError(t, err)
				assert.Equal(t, newname, d.DocName)

				newname = nfc
				_, err = vfs.ModifyDirMetadata(fs, doc, &vfs.DocPatch{
					Name: &newname,
				})
				require.NoError(t, err)
				d, err = fs.DirByPath("/" + newname)
				require.NoError(t, err)
				assert.Equal(t, newname, d.DocName)
			})

			t.Run("Walk", func(t *testing.T) {
				walktree := H{
					"walk/": H{
						"dirchild1/": H{
							"food/": H{},
							"bard/": H{},
						},
						"dirchild2/": H{
							"foof": nil,
							"barf": nil,
						},
						"dirchild3/": H{},
						"filechild1": nil,
					},
				}

				_ = createTree(t, fs, walktree, consts.RootDirID)

				walked := H{}
				err := vfs.Walk(fs, "/walk", func(name string, dir *vfs.DirDoc, file *vfs.FileDoc, err error) error {
					if !assert.NoError(t, err) {
						return err
					}

					if dir != nil && !assert.Equal(t, dir.Fullpath, name) {
						return fmt.Errorf("Bad fullpath")
					}

					if file != nil && !assert.True(t, strings.HasSuffix(name, file.DocName)) {
						return fmt.Errorf("Bad fullpath")
					}

					walked[name] = nil
					return nil
				})
				assert.NoError(t, err)

				expectedWalk := H{
					"/walk":                nil,
					"/walk/dirchild1":      nil,
					"/walk/dirchild1/food": nil,
					"/walk/dirchild1/bard": nil,
					"/walk/dirchild2":      nil,
					"/walk/dirchild2/foof": nil,
					"/walk/dirchild2/barf": nil,
					"/walk/dirchild3":      nil,
					"/walk/filechild1":     nil,
				}

				assert.Equal(t, expectedWalk, walked)
			})

			t.Run("WalkAlreadyLocked", func(t *testing.T) {
				walktree := H{
					"walk2/": H{
						"dirchild1/": H{
							"food/": H{},
							"bard/": H{},
						},
						"dirchild2/": H{
							"foof": nil,
							"barf": nil,
						},
						"dirchild3/": H{},
						"filechild1": nil,
					},
				}

				_ = createTree(t, fs, walktree, consts.RootDirID)

				done := make(chan bool)

				go func() {
					dir, err := fs.DirByPath("/walk2")
					assert.NoError(t, err)

					assert.NoError(t, mutex.Lock())
					defer mutex.Unlock()

					err = vfs.WalkAlreadyLocked(fs, dir, func(_ string, _ *vfs.DirDoc, _ *vfs.FileDoc, err error) error {
						assert.NoError(t, err)
						return err
					})
					assert.NoError(t, err)
					done <- true
				}()

				select {
				case <-done:
					return
				case <-time.After(3 * time.Second):
					t.Fatal("deadline: WalkAlreadyLocked is probably trying to acquire the VFS lock")
				}
			})

			t.Run("ContentDisposition", func(t *testing.T) {
				foo := vfs.ContentDisposition("inline", "foo.jpg")
				assert.Equal(t, `inline; filename="foo.jpg"`, foo)
				space := vfs.ContentDisposition("inline", "foo bar.jpg")
				assert.Equal(t, `inline; filename="foobar.jpg"; filename*=UTF-8''foo%20bar.jpg`, space)
				accents := vfs.ContentDisposition("inline", "héçà")
				assert.Equal(t, `inline; filename="h"; filename*=UTF-8''h%C3%A9%C3%A7%C3%A0`, accents)
				tab := vfs.ContentDisposition("inline", "tab\t")
				assert.Equal(t, `inline; filename="tab"; filename*=UTF-8''tab%09`, tab)
				emoji := vfs.ContentDisposition("inline", "🐧")
				assert.Equal(t, `inline; filename="download"; filename*=UTF-8''%F0%9F%90%A7`, emoji)
			})

			t.Run("Archive", func(t *testing.T) {
				tree := H{
					"archive/": H{
						"foo.jpg":    nil,
						"foobar.jpg": nil,
						"hello.jpg":  nil,
						"bar/": H{
							"baz/": H{
								"one.png": nil,
								"two.png": nil,
							},
							"z.gif": nil,
						},
						"qux/": H{
							"quux":   nil,
							"courge": nil,
						},
					},
				}
				dirdoc := createTree(t, fs, tree, consts.RootDirID)

				foobar, err := fs.FileByPath("/archive/foobar.jpg")
				assert.NoError(t, err)

				a := &vfs.Archive{
					Name: "test",
					IDs: []string{
						foobar.ID(),
					},
					Files: []string{
						"/archive/foo.jpg",
						"/archive/bar",
					},
				}
				w := httptest.NewRecorder()
				err = a.Serve(fs, w)
				assert.NoError(t, err)

				res := w.Result()
				disposition := res.Header.Get("Content-Disposition")
				assert.Equal(t, `attachment; filename="test.zip"`, disposition)
				assert.Equal(t, "application/zip", res.Header.Get("Content-Type"))

				b, err := io.ReadAll(res.Body)
				assert.NoError(t, err)
				z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
				assert.NoError(t, err)
				assert.Equal(t, 7, len(z.File))
				zipfiles := H{}
				for _, f := range z.File {
					zipfiles[f.Name] = nil
				}
				assert.EqualValues(t, H{
					"test/foobar.jpg":      nil,
					"test/foo.jpg":         nil,
					"test/bar/":            nil,
					"test/bar/baz/":        nil,
					"test/bar/baz/one.png": nil,
					"test/bar/baz/two.png": nil,
					"test/bar/z.gif":       nil,
				}, zipfiles)
				assert.NoError(t, fs.DestroyDirAndContent(dirdoc, fs.EnsureErased))
			})

			t.Run("CreateFileTooBig", func(t *testing.T) {
				diskQuota = 1 << (1 * 10) // 1KB
				defer func() { diskQuota = 0 }()

				diskUsage1, err := fs.DiskUsage()
				require.NoError(t, err)

				doc1, err := vfs.NewFileDoc(
					"too-big",
					consts.RootDirID,
					diskQuota+1,
					nil,
					"application/octet-stream",
					"application",
					time.Now(),
					false,
					false,
					false,
					nil,
				)
				require.NoError(t, err)

				_, err = fs.CreateFile(doc1, nil)
				assert.Equal(t, vfs.ErrFileTooBig, err)

				doc2, err := vfs.NewFileDoc(
					"too-big",
					consts.RootDirID,
					diskQuota/2,
					nil,
					"application/octet-stream",
					"application",
					time.Now(),
					false,
					false,
					false,
					nil,
				)
				require.NoError(t, err)

				f, err := fs.CreateFile(doc2, nil)
				assert.NoError(t, err)
				assert.Error(t, f.Close())

				_, err = fs.FileByPath("/too-big")
				assert.True(t, os.IsNotExist(err))

				doc3, err := vfs.NewFileDoc(
					"too-big",
					consts.RootDirID,
					diskQuota/2,
					nil,
					"application/octet-stream",
					"application",
					time.Now(),
					false,
					false,
					false,
					nil,
				)
				require.NoError(t, err)

				f, err = fs.CreateFile(doc3, nil)
				assert.NoError(t, err)
				_, err = io.Copy(f, bytes.NewReader(crypto.GenerateRandomBytes(int(doc3.ByteSize))))
				assert.NoError(t, err)
				err = f.Close()
				assert.NoError(t, err)

				diskUsage2, err := fs.DiskUsage()
				assert.NoError(t, err)
				assert.Equal(t, diskUsage1+diskQuota/2, diskUsage2)

				doc4, err := vfs.NewFileDoc(
					"too-big2",
					consts.RootDirID,
					-1,
					nil,
					"application/octet-stream",
					"application",
					time.Now(),
					false,
					false,
					false,
					nil,
				)
				require.NoError(t, err)

				f, err = fs.CreateFile(doc4, nil)
				assert.NoError(t, err)
				_, err = io.Copy(f, bytes.NewReader(crypto.GenerateRandomBytes(int(diskQuota/2+1))))
				assert.Error(t, err)
				assert.Equal(t, vfs.ErrFileTooBig, err)
				err = f.Close()
				assert.Error(t, err)
				assert.Equal(t, vfs.ErrFileTooBig, err)

				_, err = fs.FileByPath("/too-big2")
				assert.True(t, os.IsNotExist(err))

				root, err := fs.DirByPath("/")
				require.NoError(t, err)

				assert.NoError(t, fs.DestroyDirContent(root, fs.EnsureErased))
			})

			t.Run("CreateFileDocCopy", func(t *testing.T) {
				md5sum := []byte("md5sum")
				file, err := vfs.NewFileDoc("file", consts.RootDirID, -1, md5sum, "foo/bar", "foo", time.Now(), false, false, false, []string{})
				require.NoError(t, err)

				newname := "file (copy).txt"
				newdoc := vfs.CreateFileDocCopy(file, "12345", newname)
				assert.Empty(t, newdoc.DocID)
				assert.Empty(t, newdoc.DocRev)
				assert.Equal(t, "12345", newdoc.DirID)
				assert.Equal(t, newname, newdoc.DocName)
				assert.Equal(t, "text/plain", newdoc.Mime)
				assert.Equal(t, "text", newdoc.Class)
				assert.Equal(t, file.ByteSize, newdoc.ByteSize)
				assert.Equal(t, file.MD5Sum, newdoc.MD5Sum)
				assert.NotEqual(t, file.CreatedAt, newdoc.CreatedAt)
				assert.Empty(t, newdoc.ReferencedBy)
			})

			t.Run("ConflictName", func(t *testing.T) {
				tree := H{"existing": nil}
				_ = createTree(t, fs, tree, consts.RootDirID)

				newname := vfs.ConflictName(fs, consts.RootDirID, "existing", true)
				assert.Equal(t, "existing (2)", newname)

				tree = H{"existing (2)": nil}
				_ = createTree(t, fs, tree, consts.RootDirID)

				newname = vfs.ConflictName(fs, consts.RootDirID, "existing", true)
				assert.Equal(t, "existing (3)", newname)

				tree = H{"existing (3)": nil}
				_ = createTree(t, fs, tree, consts.RootDirID)

				newname = vfs.ConflictName(fs, consts.RootDirID, "existing (3)", true)
				assert.Equal(t, "existing (4)", newname)

				tree = H{"existing (copy)": nil}
				_ = createTree(t, fs, tree, consts.RootDirID)

				newname = vfs.ConflictName(fs, consts.RootDirID, "existing (copy)", true)
				assert.Equal(t, "existing (copy) (2)", newname)
			})

			t.Run("CheckAvailableSpace", func(t *testing.T) {
				diskQuota = 0

				doc, err := vfs.NewFileDoc("toto", consts.RootDirID, 100, nil, "foo/bar", "foo", time.Now(), false, false, false, []string{})
				require.NoError(t, err)
				_, _, _, err = vfs.CheckAvailableDiskSpace(fs, doc)
				require.NoError(t, err)

				diskQuota = 100

				doc, err = vfs.NewFileDoc("toto", consts.RootDirID, 100, nil, "foo/bar", "foo", time.Now(), false, false, false, []string{})
				require.NoError(t, err)
				_, _, _, err = vfs.CheckAvailableDiskSpace(fs, doc)
				require.NoError(t, err)

				doc, err = vfs.NewFileDoc("toto", consts.RootDirID, 101, nil, "foo/bar", "foo", time.Now(), false, false, false, []string{})
				require.NoError(t, err)
				_, _, _, err = vfs.CheckAvailableDiskSpace(fs, doc)
				assert.Error(t, err)
				assert.Equal(t, vfs.ErrFileTooBig, err)

				maxFileSize := fs.MaxFileSize()
				if maxFileSize > 0 {
					doc, err = vfs.NewFileDoc("toto", consts.RootDirID, maxFileSize+1, nil, "foo/bar", "foo", time.Now(), false, false, false, []string{})
					require.NoError(t, err)
					_, _, _, err = vfs.CheckAvailableDiskSpace(fs, doc)
					assert.Error(t, err)
					assert.Equal(t, vfs.ErrMaxFileSize, err)
				}
			})
		})

		t.Run("UpdateFileMetadataField", func(t *testing.T) {
			// Create a simple file
			file, err := vfs.NewFileDoc("meta-file", consts.RootDirID, 0, nil, "text/plain", "text", time.Now(), false, false, false, nil)
			require.NoError(t, err)
			f, err := fs.CreateFile(file, nil)
			require.NoError(t, err)
			f.Close()
			// Ensure initial metadata is empty
			assert.Empty(t, file.Metadata)
			// Prepare metadata update via DocPatch
			newMeta := vfs.Metadata{"k1": "v1", "k2": 2}
			patch := &vfs.DocPatch{Metadata: &newMeta}
			updated, err := vfs.ModifyFileMetadata(fs, file, patch)
			require.NoError(t, err)
			assert.Equal(t, "v1", updated.Metadata["k1"])
			assert.Equal(t, 2, updated.Metadata["k2"])
			// Replace metadata with a new map
			replace := vfs.Metadata{"only": true}
			patch = &vfs.DocPatch{Metadata: &replace}
			updated, err = vfs.ModifyFileMetadata(fs, updated, patch)
			require.NoError(t, err)
			assert.Equal(t, 1, len(updated.Metadata))
			assert.Equal(t, true, updated.Metadata["only"])
		})

		t.Run("UpdateDirMetadataField", func(t *testing.T) {
			// Create a simple directory
			dir, err := vfs.NewDirDoc(fs, "meta-dir", consts.RootDirID, nil)
			require.NoError(t, err)
			require.NoError(t, fs.CreateDir(dir))

			// Ensure initial metadata is empty
			assert.Empty(t, dir.Metadata)

			// Prepare metadata update via DocPatch
			newMeta := vfs.Metadata{"k1": "v1", "k2": 2}
			patch := &vfs.DocPatch{Metadata: &newMeta}
			updated, err := vfs.ModifyDirMetadata(fs, dir, patch)
			require.NoError(t, err)
			assert.Equal(t, "v1", updated.Metadata["k1"])
			assert.Equal(t, 2, updated.Metadata["k2"])

			// Replace metadata with a new map
			replace := vfs.Metadata{"only": true}
			patch = &vfs.DocPatch{Metadata: &replace}
			updated, err = vfs.ModifyDirMetadata(fs, updated, patch)
			require.NoError(t, err)
			assert.Equal(t, 1, len(updated.Metadata))
			assert.Equal(t, true, updated.Metadata["only"])
		})
	}
}

func (d *diskImpl) DiskQuota() int64 {
	return diskQuota
}

func (h H) String() string {
	return printH(h, "", 0)
}

func printH(h H, str string, count int) string {
	for name, hh := range h {
		for i := 0; i < count; i++ {
			str += "\t"
		}
		str += fmt.Sprintf("%s:\n", name)
		str += printH(hh, "", count+1)
	}
	return str
}

func createTree(t *testing.T, fs vfs.VFS, tree H, dirID string) *vfs.DirDoc {
	t.Helper()

	if tree == nil {
		return nil
	}

	if dirID == "" {
		dirID = consts.RootDirID
	}

	var err error
	var dirdoc *vfs.DirDoc
	for name, children := range tree {
		if name[len(name)-1] == '/' {
			dirdoc, err = vfs.NewDirDoc(fs, name[:len(name)-1], dirID, nil)
			require.NoError(t, err)

			err = fs.CreateDir(dirdoc)
			require.NoError(t, err)

			createTree(t, fs, children, dirdoc.ID())
		} else {
			mime, class := vfs.ExtractMimeAndClassFromFilename(name)
			filedoc, err := vfs.NewFileDoc(name, dirID, -1, nil, mime, class, time.Now(), false, false, false, nil)
			require.NoError(t, err)

			f, err := fs.CreateFile(filedoc, nil)
			require.NoError(t, err)

			err = f.Close()
			require.NoError(t, err)
		}
	}
	return dirdoc
}

func fetchTree(fs vfs.VFS, root string) (H, error) {
	parent, err := fs.DirByPath(root)
	if err != nil {
		return nil, err
	}
	h, err := recFetchTree(fs, parent, path.Clean(root))
	if err != nil {
		return nil, err
	}
	hh := make(H)
	hh[parent.DocName+"/"] = h
	return hh, nil
}

func recFetchTree(fs vfs.VFS, parent *vfs.DirDoc, name string) (H, error) {
	h := make(H)
	iter := fs.DirIterator(parent, nil)
	for {
		d, f, err := iter.Next()
		if errors.Is(err, vfs.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		if d != nil {
			if path.Join(name, d.DocName) != d.Fullpath {
				return nil, fmt.Errorf("Bad fullpath: %s instead of %s", d.Fullpath, path.Join(name, d.DocName))
			}
			children, err := recFetchTree(fs, d, d.Fullpath)
			if err != nil {
				return nil, err
			}
			h[d.DocName+"/"] = children
		} else {
			h[f.DocName] = nil
		}
	}
	return h, nil
}

func (c *contexter) DBCluster() int         { return c.cluster }
func (c *contexter) DomainName() string     { return c.domain }
func (c *contexter) DBPrefix() string       { return c.prefix }
func (c *contexter) GetContextName() string { return c.context }

func makeAferoFS(t *testing.T) vfs.VFS {
	t.Helper()

	tempdir := t.TempDir()

	db := &contexter{0, "swift.testvfs.example.org", "swift.testvfs.example.org", "cozy_beta"}
	index := vfs.NewCouchdbIndexer(db)
	mutex = config.Lock().ReadWrite(db, "vfs-afero-test")
	aferoFs, err := vfsafero.New(db, index, &diskImpl{}, mutex,
		&url.URL{Scheme: "file", Host: "localhost", Path: tempdir}, "io.cozy.vfs.test")
	require.NoError(t, err)

	require.NoError(t, couchdb.ResetDB(db, consts.Files))
	t.Cleanup(func() { _ = couchdb.DeleteDB(db, consts.Files) })

	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Files))
	couchdb.DefineViews(g, db, couchdb.ViewsByDoctype(consts.Files))

	require.NoError(t, g.Wait())
	require.NoError(t, aferoFs.InitFs())

	return aferoFs
}

func makeSwiftFS(t *testing.T) vfs.VFS {
	t.Helper()

	db := &contexter{0, "io.cozy.vfs.test", "io.cozy.vfs.test", "cozy_beta"}
	index := vfs.NewCouchdbIndexer(db)

	swiftSrv, err := swifttest.NewSwiftServer("localhost")
	require.NoError(t, err, "failed to create swift server")

	require.NoError(t, config.InitSwiftConnection(config.Fs{
		URL: &url.URL{
			Scheme:   "swift",
			Host:     "localhost",
			RawQuery: "UserName=swifttest&Password=swifttest&AuthURL=" + url.QueryEscape(swiftSrv.AuthURL),
		},
	}))

	mutex = config.Lock().ReadWrite(db, "vfs-swiftv3-test")
	swiftFs, err := vfsswift.NewV3(db, index, &diskImpl{}, mutex)
	require.NoError(t, err)

	require.NoError(t, couchdb.ResetDB(db, consts.Files))

	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Files))
	couchdb.DefineViews(g, db, couchdb.ViewsByDoctype(consts.Files))
	require.NoError(t, g.Wait())

	require.NoError(t, swiftFs.InitFs())

	t.Cleanup(func() {
		_ = couchdb.DeleteDB(db, consts.Files)
		if swiftSrv != nil {
			swiftSrv.Close()
		}
	})

	return swiftFs
}

func makeS3FS(t *testing.T) vfs.VFS {
	t.Helper()

	minioFixture := testutils.StartMinio(t)
	db := &contexter{0, "io.cozy.vfs.s3.test", "io.cozy.vfs.s3.test", "cozy_beta"}
	index := vfs.NewCouchdbIndexer(db)

	require.NoError(t, config.InitS3Connection(config.Fs{
		URL: minioFixture.FsURL("test"),
	}))

	mutex = config.Lock().ReadWrite(db, "vfs-s3-test")
	s3Fs, err := vfss3.New(db, index, &diskImpl{}, mutex)
	require.NoError(t, err)

	require.NoError(t, couchdb.ResetDB(db, consts.Files))
	t.Cleanup(func() { _ = couchdb.DeleteDB(db, consts.Files) })

	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Files))
	couchdb.DefineViews(g, db, couchdb.ViewsByDoctype(consts.Files))

	require.NoError(t, g.Wait())
	require.NoError(t, s3Fs.InitFs())

	return s3Fs
}
