package vfss3_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

type deleteTestDB struct{}

func (deleteTestDB) DBCluster() int         { return 0 }
func (deleteTestDB) DomainName() string     { return "io.cozy.vfss3.delete.test" }
func (deleteTestDB) DBPrefix() string       { return "io.cozy.vfss3.delete.test" }
func (deleteTestDB) GetContextName() string { return "default" }

type listCounter struct {
	next  http.RoundTripper
	lists atomic.Int64
}

func (c *listCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	q := r.URL.Query()
	if r.Method == http.MethodGet && (q.Has("list-type") || q.Has("prefix")) {
		c.lists.Add(1)
	}
	return c.next.RoundTrip(r)
}

func TestDeleteWithoutListing(t *testing.T) {
	if testing.Short() {
		t.Skip("CouchDB and Docker are required for this test")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	off := false
	config.GetConfig().Fs.S3.DeleteInstancesByListing = &off
	t.Cleanup(func() { config.GetConfig().Fs.S3.DeleteInstancesByListing = nil })

	mf := testutils.StartMinio(t)
	counter := &listCounter{next: http.DefaultTransport}
	require.NoError(t, config.InitS3Connection(config.Fs{
		URL:       mf.FsURL(),
		Transport: counter,
		S3:        config.FsS3{Buckets: map[string]config.FsS3Bucket{"default": {Name: "delete-test"}}},
	}))
	storage := config.GetS3Storage(config.S3StorageFiles)
	ctx := context.Background()

	db := deleteTestDB{}
	for _, doctype := range []string{consts.Files, consts.FilesVersions, consts.NotesImages, consts.Jobs} {
		require.NoError(t, couchdb.ResetDB(db, doctype))
		t.Cleanup(func() { _ = couchdb.DeleteDB(db, doctype) })
	}
	g, _ := errgroup.WithContext(ctx)
	couchdb.DefineIndexes(g, db, couchdb.IndexesByDoctype(consts.Files))
	couchdb.DefineViews(g, db, couchdb.ViewsByDoctype(consts.Files))
	require.NoError(t, g.Wait())

	index := vfs.NewCouchdbIndexer(db)
	fs, err := vfss3.New(db, index, writeContentAtDisk{}, lock.NewInMemory().ReadWrite(db, "vfs"))
	require.NoError(t, err)
	require.NoError(t, fs.InitFs())

	keyPrefix := storage.Prefix + db.DBPrefix() + "/"
	thumbs := vfss3.NewThumbsFs(storage.Client, storage.Bucket, keyPrefix)
	put := func(key string) {
		_, err := storage.Client.PutObject(ctx, storage.Bucket, key, bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{})
		require.NoError(t, err)
	}
	write := func(w io.WriteCloser) {
		_, err := w.Write([]byte("content"))
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}
	createFile := func(name string) *vfs.FileDoc {
		doc, err := vfs.NewFileDoc(name, "", -1, nil, "image/jpeg", "image", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := fs.CreateFile(doc, nil)
		require.NoError(t, err)
		write(f)
		return doc
	}

	photo := createFile("photo.jpg")
	for _, format := range vfs.ThumbnailFormatNames {
		th, err := thumbs.CreateThumb(photo, format)
		require.NoError(t, err)
		_, err = th.Write([]byte("thumb"))
		require.NoError(t, err)
		require.NoError(t, th.Commit())
	}
	version := vfs.NewVersion(photo)
	version.DocID = photo.ID() + "/" + vfss3.NewInternalID()
	version.ByteSize = 3
	require.NoError(t, fs.ImportFileVersion(version, io.NopCloser(bytes.NewReader([]byte("old")))))

	_, err = vfs.TrashFile(fs, createFile("trashed.jpg"))
	require.NoError(t, err)

	avatar, err := vfss3.NewAvatarFs(storage.Client, storage.Bucket, keyPrefix).CreateAvatar("image/png")
	require.NoError(t, err)
	write(avatar)

	noteImageID := "0123456789abcdef0123456789abcdef/0190f0a0-0000-7000-8000-000000000000"
	for _, format := range []string{consts.NoteImageOriginalFormat, consts.NoteImageThumbFormat} {
		th, err := thumbs.CreateNoteThumb(noteImageID, "image/png", format)
		require.NoError(t, err)
		_, err = th.Write([]byte("note image"))
		require.NoError(t, err)
		require.NoError(t, th.Commit())
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(db, &couchdb.JSONDoc{
		Type: consts.NotesImages,
		M:    map[string]interface{}{"_id": noteImageID, "name": "image.png"},
	}))

	pendingID := "fedcba9876543210fedcba9876543210"
	pendingErase := vfss3.MakeObjectKey(keyPrefix, pendingID, vfss3.NewInternalID())
	put(pendingErase)
	put(keyPrefix + "thumbs/fedcba9876543210fedcba/98765/43210-tiny")
	otherInstance := storage.Prefix + "other.instance/kept"
	put(otherInstance)
	require.NoError(t, couchdb.CreateDoc(db, &couchdb.JSONDoc{
		Type: consts.Jobs,
		M: map[string]interface{}{
			"worker": "trash-files",
			"state":  "queued",
			"message": map[string]interface{}{
				"ids":     []string{pendingID},
				"objects": []string{pendingErase, otherInstance},
			},
		},
	}))

	listObjects := func(prefix string) []string {
		var keys []string
		for obj := range storage.Client.ListObjects(ctx, storage.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
			require.NoError(t, obj.Err)
			keys = append(keys, obj.Key)
		}
		return keys
	}
	// photo, its 4 thumbnails and old version, trashed file, avatar, 2 note
	// image formats, object and thumbnail awaiting a trash-files job
	require.Len(t, listObjects(keyPrefix), 12)

	before := counter.lists.Load()
	require.NoError(t, fs.Delete())
	assert.Equal(t, before, counter.lists.Load(), "Delete must not list the bucket")

	assert.Empty(t, listObjects(keyPrefix))
	assert.Equal(t, []string{otherInstance}, listObjects(storage.Prefix+"other.instance/"))
}
