package vfs_test

import (
	"context"
	"crypto/md5"
	"errors"
	"io"
	"math/rand"
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
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
	"github.com/ncw/swift/v2"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// testPartSize is above the minimal part on S3 (5 MiB), and on Swift (5 GiB
// in 900 segments).
const testPartSize = 6 << 20

// uploadBackend gives a VFS for the tests of the resumable uploads, and a way
// to count what an upload has left in the storage.
type uploadBackend struct {
	name string
	// newFS returns a new VFS on the same storage and database, as a stack
	// that restarts would do.
	newFS func() vfs.VFS
	// newFSWithLock returns a VFS with its own lock, like the source of a
	// storage migration.
	newFSWithLock func(name string) vfs.VFS
	// objects returns the names of what an upload keeps in the storage
	// (segments, tails, temporary file), apart from the file itself.
	objects func(u *vfs.Upload) []string
	// pending tells if a multipart upload is still open in the storage.
	pending func(u *vfs.Upload) bool
}

func initUploadDB(t *testing.T, db *contexter) {
	require.NoError(t, couchdb.ResetDB(db, consts.Files))
	_ = couchdb.DeleteDB(db, consts.FilesUploads)
	require.NoError(t, couchdb.ResetDB(db, consts.FilesVersions))
	t.Cleanup(func() {
		_ = couchdb.DeleteDB(db, consts.Files)
		_ = couchdb.DeleteDB(db, consts.FilesUploads)
		_ = couchdb.DeleteDB(db, consts.FilesVersions)
	})
	g, _ := errgroup.WithContext(context.Background())
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Files))
	couchdb.DefineViews(g, db, couchdb.ViewsByDoctype(consts.Files))
	require.NoError(t, g.Wait())
}

func aferoUploadBackend(t *testing.T) uploadBackend {
	dir := t.TempDir()
	db := &contexter{0, "upload.afero.example.org", "upload.afero.example.org", "cozy_beta"}
	initUploadDB(t, db)
	withLock := func(name string) vfs.VFS {
		fs, err := vfsafero.New(db, vfs.NewCouchdbIndexer(db), &diskImpl{},
			config.Lock().ReadWrite(db, name),
			&url.URL{Scheme: "file", Host: "localhost", Path: dir}, "upload-test")
		require.NoError(t, err)
		return fs
	}
	newFS := func() vfs.VFS { return withLock("vfs") }
	require.NoError(t, newFS().InitFs())
	return uploadBackend{
		name:          "afero",
		newFS:         newFS,
		newFSWithLock: withLock,
		objects: func(u *vfs.Upload) []string {
			p := path.Join(dir, "upload-test", vfs.UploadsDirName, u.UploadID)
			entries, err := os.ReadDir(p)
			if err != nil {
				return nil
			}
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = path.Join(p, e.Name())
			}
			return names
		},
		pending: func(u *vfs.Upload) bool { return false },
	}
}

func s3UploadBackend(t *testing.T) uploadBackend {
	minioFixture := testutils.StartMinio(t)
	db := &contexter{0, "upload.s3.example.org", "upload.s3.example.org", "cozy_beta"}
	initUploadDB(t, db)
	require.NoError(t, config.InitS3Connection(config.Fs{
		URL: minioFixture.FsURL(),
		S3:  config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "upload-storage"}}},
	}))
	storage := config.GetS3Storage(config.S3StorageFiles)
	withLock := func(name string) vfs.VFS {
		fs, err := vfss3.New(db, vfs.NewCouchdbIndexer(db), &diskImpl{}, config.Lock().ReadWrite(db, name))
		require.NoError(t, err)
		return fs
	}
	newFS := func() vfs.VFS { return withLock("vfs") }
	require.NoError(t, newFS().InitFs())
	keyPrefix := storage.Prefix + db.DBPrefix() + "/"
	return uploadBackend{
		name:          "s3",
		newFS:         newFS,
		newFSWithLock: withLock,
		objects: func(u *vfs.Upload) []string {
			var names []string
			for obj := range storage.Client.ListObjects(context.Background(), storage.Bucket, minio.ListObjectsOptions{
				Prefix:    keyPrefix + vfs.UploadsObjectPrefix + u.UploadID + "/",
				Recursive: true,
			}) {
				require.NoError(t, obj.Err)
				names = append(names, obj.Key)
			}
			return names
		},
		pending: func(u *vfs.Upload) bool {
			core := minio.Core{Client: storage.Client}
			res, err := core.ListMultipartUploads(context.Background(), storage.Bucket, keyPrefix, "", "", "", 1000)
			require.NoError(t, err)
			for _, up := range res.Uploads {
				if up.UploadID == u.StorageID {
					return true
				}
			}
			return false
		},
	}
}

func swiftUploadBackend(t *testing.T) uploadBackend {
	return swiftUploadBackendAt(t, testutils.StartSwift(t))
}

func swiftUploadBackendAt(t *testing.T, swiftFixture *testutils.SwiftFixture) uploadBackend {
	db := &contexter{0, "upload.swift.example.org", "upload.swift.example.org", "cozy_beta"}
	initUploadDB(t, db)
	require.NoError(t, config.InitSwiftConnection(config.Fs{URL: swiftFixture.FsURL()}))
	conn := config.GetSwiftConnection()
	withLock := func(name string) vfs.VFS {
		fs, err := vfsswift.NewV3(db, vfs.NewCouchdbIndexer(db), &diskImpl{}, config.Lock().ReadWrite(db, name))
		require.NoError(t, err)
		return fs
	}
	newFS := func() vfs.VFS { return withLock("vfs") }
	require.NoError(t, newFS().InitFs())
	container := "cozy-v3-" + db.DBPrefix()
	return uploadBackend{
		name:          "swift",
		newFS:         newFS,
		newFSWithLock: withLock,
		objects: func(u *vfs.Upload) []string {
			names, err := conn.ObjectNamesAll(context.Background(), container, &swift.ObjectsOpts{
				Prefix: vfs.UploadsObjectPrefix + u.UploadID + "/",
			})
			require.NoError(t, err)
			return names
		},
		pending: func(u *vfs.Upload) bool { return false },
	}
}

func uploadBackends(t *testing.T) []uploadBackend {
	return []uploadBackend{aferoUploadBackend(t), s3UploadBackend(t), swiftUploadBackend(t)}
}

func setUploadConfig(t *testing.T, maxPerInstance int) {
	cfg := &config.GetConfig().Fs.ResumableUploads
	old := *cfg
	cfg.PartSize = testPartSize
	cfg.MaxAge = time.Hour
	cfg.MaxPerInstance = maxPerInstance
	t.Cleanup(func() { *cfg = old })
}

func randomContent(size int, seed int64) []byte {
	buf := make([]byte, size)
	_, _ = rand.New(rand.NewSource(seed)).Read(buf)
	return buf
}

// cutReader returns err after n bytes, like a connection that drops.
type cutReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, c.err
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

var errCut = errors.New("connection lost")

func newUploadDoc(t *testing.T, name string, content []byte) *vfs.FileDoc {
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil, "application/octet-stream", "files",
		time.Now(), false, false, false, nil)
	require.NoError(t, err)
	if content != nil {
		sum := md5.Sum(content)
		doc.MD5Sum = sum[:]
	}
	return doc
}

func readContent(t *testing.T, fs vfs.VFS, doc *vfs.FileDoc) []byte {
	f, err := fs.OpenFile(doc)
	require.NoError(t, err)
	defer f.Close()
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	return data
}

func expireUpload(t *testing.T, fs vfs.VFS, id string) {
	u := &vfs.Upload{}
	require.NoError(t, couchdb.GetDoc(fs, consts.FilesUploads, id, u))
	u.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, couchdb.UpdateDoc(fs, u))
}

func tails(names []string) []string {
	var out []string
	for _, name := range names {
		if strings.Contains(path.Base(name), "tail-") {
			out = append(out, name)
		}
	}
	return out
}
