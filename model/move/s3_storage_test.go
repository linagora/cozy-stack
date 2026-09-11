package move

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	"github.com/cozy/cozy-stack/pkg/assets/model"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/previewfs"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3SharedStorage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a MinIO container")
	}
	config.UseTestFile(t)
	fixture := testutils.StartMinio(t)
	cfg := config.GetConfig()
	previousFS := cfg.Fs
	t.Cleanup(func() { cfg.Fs = previousFS })
	cfg.Fs = config.Fs{URL: fixture.FsURL(), S3: config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "shared-storage"}}}}
	require.NoError(t, config.InitS3Connection(cfg.Fs))
	disabled := false
	cfg.Fs.S3.AutoCreateBuckets = &disabled
	require.NoError(t, config.InitS3Connection(cfg.Fs))
	storage := config.GetS3Storage("files")
	ctx := context.Background()
	buckets, err := storage.Client.ListBuckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, storage.Bucket, buckets[0].Name)

	read := func(r io.ReadCloser, err error) string {
		t.Helper()
		require.NoError(t, err)
		b, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		return string(b)
	}
	write := func(w io.WriteCloser, content string) {
		t.Helper()
		_, err := io.WriteString(w, content)
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}
	keys := func() []string {
		t.Helper()
		var names []string
		for obj := range storage.Client.ListObjects(ctx, storage.Bucket, minio.ListObjectsOptions{Recursive: true}) {
			require.NoError(t, obj.Err)
			names = append(names, obj.Key)
		}
		return names
	}

	file := filepath.Join(t.TempDir(), "index.js")
	require.NoError(t, os.WriteFile(file, []byte("app"), 0600))
	stat, err := os.Stat(file)
	require.NoError(t, err)
	for _, kind := range []string{"apps_web", "apps_konnectors"} {
		s := config.GetS3Storage(kind)
		copier := appfs.NewS3Copier(s.Client, s.Bucket, s.Prefix)
		exists, err := copier.Start("same", "1.0.0", "")
		require.NoError(t, err)
		assert.False(t, exists)
		require.NoError(t, copier.Copy(stat, strings.NewReader(kind)))
		require.NoError(t, copier.Commit())
		exists, err = copier.Exist("same", "1.0.0", "")
		require.NoError(t, err)
		assert.True(t, exists)
		server := appfs.NewS3FileServer(s.Client, s.Bucket, s.Prefix)
		assert.Equal(t, kind, read(server.Open("same", "1.0.0", "", "index.js")))
		names, err := server.FilesList("same", "1.0.0", "")
		require.NoError(t, err)
		assert.Equal(t, []string{"index.js"}, names)
		for range 2 { // Generate the tarball, then read the cached tarball.
			response := httptest.NewRecorder()
			require.NoError(t, server.ServeCodeTarball(response, httptest.NewRequest(http.MethodGet, "/", nil), "same", "1.0.0", ""))
			assert.Equal(t, http.StatusOK, response.Code)
			assert.NotEmpty(t, response.Body.Bytes())
		}
		assert.Contains(t, keys(), s.Prefix+"same/1.0.0.cozy-installed")
		assert.Contains(t, keys(), s.Prefix+"same/1.0.0.tgz")
		_, err = copier.Start("same", "aborted", "")
		require.NoError(t, err)
		require.NoError(t, copier.Copy(stat, strings.NewReader("discard")))
		require.NoError(t, copier.Abort())
		assert.NotContains(t, keys(), s.Prefix+"same/aborted/index.js")
	}

	assets, err := dynamic.NewS3FS()
	require.NoError(t, err)
	asset := model.NewAsset(model.AssetOption{Context: "same", Name: "/index.js", Shasum: "test-asset-shasum", IsCustom: true}, []byte("asset"), nil)
	require.NoError(t, assets.Add("same", "/index.js", asset))
	content, err := assets.Get("same", "/index.js")
	require.NoError(t, err)
	assert.Equal(t, "asset", string(content))
	listed, err := assets.List()
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Len(t, listed["same"], 1)
	assert.Equal(t, "/index.js", listed["same"][0].Name)

	cache := previewfs.SystemCache()
	checksum := bytes.Repeat([]byte{1}, 16)
	require.NoError(t, cache.SetIcon(checksum, bytes.NewBufferString("icon")))
	require.NoError(t, cache.SetPreview(checksum, bytes.NewBufferString("preview")))
	icon, err := cache.GetIcon(checksum)
	require.NoError(t, err)
	assert.Equal(t, "icon", icon.String())
	preview, err := cache.GetPreview(checksum)
	require.NoError(t, err)
	assert.Equal(t, "preview", preview.String())

	var alice vfs.VFS
	for _, domain := range []string{"alice", "bob"} {
		inst := &instance.Instance{Domain: domain}
		index := &s3StorageIndexer{}
		fs, err := vfss3.New(inst, index, index, lock.NewInMemory().ReadWrite(inst, "vfs"))
		require.NoError(t, err)
		require.NoError(t, fs.InitFs())
		doc := &vfs.FileDoc{DocID: "file", DocName: "file.txt", ByteSize: int64(len(domain)), Mime: "text/plain"}
		f, err := fs.CreateFile(doc, nil)
		require.NoError(t, err)
		write(f, domain)
		assert.Equal(t, domain, read(fs.OpenFile(doc)))
		copyDoc := &vfs.FileDoc{DocName: "copy.txt", ByteSize: doc.ByteSize, Mime: doc.Mime}
		require.NoError(t, fs.CopyFile(doc, copyDoc))
		assert.Equal(t, domain, read(fs.OpenFile(copyDoc)))
		version := &vfs.Version{DocID: "file/version", ByteSize: 3}
		require.NoError(t, fs.ImportFileVersion(version, io.NopCloser(strings.NewReader("old"))))
		assert.Equal(t, "old", read(fs.OpenFileVersion(doc, version)))
		avatar, err := inst.AvatarFS().CreateAvatar("text/plain")
		require.NoError(t, err)
		write(avatar, domain)
		thumbs := inst.ThumbsFS()
		thumb, err := thumbs.CreateNoteThumb("same", "text/plain", "small")
		require.NoError(t, err)
		_, err = io.WriteString(thumb, domain)
		require.NoError(t, err)
		require.NoError(t, thumb.Commit())
		assert.Equal(t, domain, read(thumbs.OpenNoteThumb("same", "small")))
		if domain == "alice" {
			alice = fs
		}
	}

	archiver := newS3Archiver()
	export := &ExportDoc{DocID: "archive", Domain: "alice"}
	archive, err := archiver.CreateArchive(export)
	require.NoError(t, err)
	write(archive, "export")
	require.Eventually(t, func() bool {
		_, err := storage.Client.StatObject(ctx, storage.Bucket, "exports/alice/archive", minio.StatObjectOptions{})
		return err == nil
	}, 10*time.Second, 50*time.Millisecond)
	assert.Equal(t, "export", read(archiver.OpenArchive(nil, export)))

	before := keys()
	require.NoError(t, alice.Delete())
	var remaining []string
	for _, key := range before {
		if !strings.HasPrefix(key, "files/alice/") {
			remaining = append(remaining, key)
		}
	}
	assert.Equal(t, remaining, keys(), "deleting an instance must preserve all other storage")
	require.NoError(t, assets.Remove("same", "/index.js"))
	require.NoError(t, archiver.RemoveArchives([]*ExportDoc{export}))
	var afterRemoval []string
	for _, key := range remaining {
		if key != "assets/same/index.js" && key != "exports/alice/archive" {
			afterRemoval = append(afterRemoval, key)
		}
	}
	assert.Equal(t, afterRemoval, keys(), "asset and export deletion must preserve other objects")
}

type s3StorageIndexer struct{ vfs.Indexer }

func (i *s3StorageIndexer) InitIndex() error                            { return nil }
func (i *s3StorageIndexer) DiskQuota() int64                            { return 0 }
func (i *s3StorageIndexer) FilePath(doc *vfs.FileDoc) (string, error)   { return "/" + doc.DocName, nil }
func (i *s3StorageIndexer) DirChildExists(string, string) (bool, error) { return false, nil }
func (i *s3StorageIndexer) CreateNamedFileDoc(*vfs.FileDoc) error       { return nil }
func (i *s3StorageIndexer) CreateVersion(*vfs.Version) error            { return nil }
