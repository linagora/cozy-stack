package vfs_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSwiftLargeObjects checks that every operation on a file keeps working
// when its object is a SLO manifest, and removes its segments with it.
func TestSwiftLargeObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)
	config.GetConfig().Fs.Versioning = config.FsVersioning{MaxNumberToKeep: 5}

	b := swiftUploadBackend(t)
	fs := b.newFS()

	upload := func(name string, content []byte, olddoc *vfs.FileDoc) (*vfs.FileDoc, *vfs.Upload) {
		doc := newUploadDoc(t, name, content)
		if olddoc != nil {
			doc.DirID = olddoc.DirID
		}
		u, err := vfs.NewUpload(fs, doc, olddoc, "", -1)
		require.NoError(t, err)
		created, err := vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(created.InternalID, "slo-"))
		return created, u
	}

	usage, err := fs.DiskUsage()
	require.NoError(t, err)
	content1 := randomContent(2*testPartSize+10, 21)
	doc, u1 := upload("large.bin", content1, nil)
	assert.Len(t, b.objects(u1), 3)

	t.Run("DiskUsageIsTheRealSize", func(t *testing.T) {
		after, err := fs.DiskUsage()
		require.NoError(t, err)
		assert.Equal(t, usage+int64(len(content1)), after)
	})

	t.Run("Range", func(t *testing.T) {
		f, err := fs.OpenFile(doc)
		require.NoError(t, err)
		defer f.Close()
		_, err = f.Seek(testPartSize-5, io.SeekStart)
		require.NoError(t, err)
		buf := make([]byte, 10)
		_, err = io.ReadFull(f, buf)
		require.NoError(t, err)
		assert.Equal(t, content1[testPartSize-5:testPartSize+5], buf)
	})

	t.Run("Fsck", func(t *testing.T) {
		var logs []*vfs.FsckLog
		require.NoError(t, fs.Fsck(func(l *vfs.FsckLog) { logs = append(logs, l) }, false))
		assert.Empty(t, logs)
	})

	copied, err := vfs.CreateFileDocCopy(doc, consts.RootDirID, "copy.bin")
	require.NoError(t, err)
	t.Run("Copy", func(t *testing.T) {
		require.NoError(t, fs.CopyFile(doc, copied))
		assert.Equal(t, content1, readContent(t, fs, copied))
	})

	content2 := randomContent(testPartSize+10, 22)
	var u2 *vfs.Upload
	t.Run("VersionsAndRevert", func(t *testing.T) {
		var err error
		doc, u2 = upload("large.bin", content2, doc)
		assert.Equal(t, content2, readContent(t, fs, doc))
		versions, err := vfs.VersionsFor(fs, doc.ID())
		require.NoError(t, err)
		require.Len(t, versions, 1)
		f, err := fs.OpenFileVersion(doc, versions[0])
		require.NoError(t, err)
		old, err := io.ReadAll(f)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		assert.Equal(t, content1, old)

		require.NoError(t, fs.RevertFileVersion(doc, versions[0]))
		doc, err = fs.FileByID(doc.ID())
		require.NoError(t, err)
		assert.Equal(t, content1, readContent(t, fs, doc))
	})

	t.Run("CleanOldVersion", func(t *testing.T) {
		versions, err := vfs.VersionsFor(fs, doc.ID())
		require.NoError(t, err)
		require.Len(t, versions, 1)
		assert.NotEmpty(t, b.objects(u2))
		require.NoError(t, fs.CleanOldVersion(doc.ID(), versions[0]))
		assert.Empty(t, b.objects(u2))
	})

	t.Run("Destroy", func(t *testing.T) {
		require.NoError(t, fs.DestroyFile(doc))
		assert.Empty(t, b.objects(u1))
		// The copy is a plain object, independent of the segments.
		assert.Equal(t, content1, readContent(t, fs, copied))
	})

	t.Run("TrashPurge", func(t *testing.T) {
		dir, err := vfs.NewDirDoc(fs, "purged", consts.RootDirID, nil)
		require.NoError(t, err)
		require.NoError(t, fs.CreateDir(dir))
		content := randomContent(testPartSize+10, 23)
		d := newUploadDoc(t, "inside.bin", content)
		d.DirID = dir.ID()
		u, err := vfs.NewUpload(fs, d, nil, "", -1)
		require.NoError(t, err)
		_, err = vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
		require.NoError(t, err)
		require.NotEmpty(t, b.objects(u))

		var journal vfs.TrashJournal
		require.NoError(t, fs.DestroyDirAndContent(dir, func(j vfs.TrashJournal) error {
			journal = j
			return nil
		}))
		require.NoError(t, fs.EnsureErased(journal))
		assert.Empty(t, b.objects(u))
	})
}

func TestSwiftLargeObjectOverwrittenWithoutVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)
	config.GetConfig().Fs.Versioning = config.FsVersioning{MaxNumberToKeep: 0}

	b := swiftUploadBackend(t)
	fs := b.newFS()
	content := randomContent(testPartSize+10, 31)
	u, err := vfs.NewUpload(fs, newUploadDoc(t, "replaced.bin", content), nil, "", -1)
	require.NoError(t, err)
	doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
	require.NoError(t, err)
	require.NotEmpty(t, b.objects(u))

	newdoc := newUploadDoc(t, doc.DocName, nil)
	newdoc.ByteSize = 2
	f, err := fs.CreateFile(newdoc, doc)
	require.NoError(t, err)
	_, err = f.Write([]byte("v2"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	versions, err := vfs.VersionsFor(fs, doc.ID())
	require.NoError(t, err)
	assert.Empty(t, versions)
	assert.Empty(t, b.objects(u))
}

// TestSwiftLargeObjectDeletedWithoutBulkDelete checks that the segments of a
// file are removed on a Swift that refuses the bulk delete.
func TestSwiftLargeObjectDeletedWithoutBulkDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	fixture := testutils.StartSwift(t)
	target, err := url.Parse("http://" + fixture.Endpoint)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("bulk-delete") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer refusing.Close()

	b := swiftUploadBackendAt(t, &testutils.SwiftFixture{Endpoint: strings.TrimPrefix(refusing.URL, "http://")})
	fs := b.newFS()
	content := randomContent(testPartSize+10, 24)
	u, err := vfs.NewUpload(fs, newUploadDoc(t, "no-bulk.bin", content), nil, "", -1)
	require.NoError(t, err)
	doc, err := vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
	require.NoError(t, err)
	require.NotEmpty(t, b.objects(u))

	require.NoError(t, fs.DestroyFile(doc))
	assert.Empty(t, b.objects(u))
}

// TestSwiftDissociatedLargeObject checks the deletion of a plain object with
// the name of a large object: a sharing dissociates a file by copying it,
// which flattens the manifest, and keeps its internal ID.
func TestSwiftDissociatedLargeObject(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setUploadConfig(t, 20)

	b := swiftUploadBackend(t)
	fs := b.newFS()
	content := randomContent(testPartSize+10, 25)
	u, err := vfs.NewUpload(fs, newUploadDoc(t, "shared.bin", content), nil, "", -1)
	require.NoError(t, err)
	src, err := vfs.AppendUpload(fs, u, bytes.NewReader(content), true)
	require.NoError(t, err)

	dst := src.Clone().(*vfs.FileDoc)
	dst.DocName = "dissociated.bin"
	dst.DocRev = ""
	require.NoError(t, fs.DissociateFile(src, dst))
	assert.Empty(t, b.objects(u))
	assert.Equal(t, content, readContent(t, fs, dst))

	require.NoError(t, fs.DestroyFile(dst))
	names, err := config.GetSwiftConnection().ObjectNamesAll(context.Background(), "cozy-v3-upload.swift.example.org", nil)
	require.NoError(t, err)
	for _, name := range names {
		assert.NotContains(t, name, dst.DocID)
	}
}
