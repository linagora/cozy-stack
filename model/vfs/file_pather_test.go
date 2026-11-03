package vfs_test

import (
	"os"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type dirByIDIndexer struct {
	vfs.Indexer
	calls []string
}

func (idx *dirByIDIndexer) DirByID(id string) (*vfs.DirDoc, error) {
	idx.calls = append(idx.calls, id)
	if id == "fetched" {
		return &vfs.DirDoc{DocID: id, Type: consts.DirType, Fullpath: "/fetched"}, nil
	}
	return nil, os.ErrNotExist
}

func TestNewFilePatherWithDirs(t *testing.T) {
	idx := &dirByIDIndexer{}
	fp := vfs.NewFilePatherWithDirs(idx, map[string]*vfs.DirDoc{
		"loaded":    {DocID: "loaded", Type: consts.DirType, Fullpath: "/loaded"},
		"missing":   nil,
		"not-a-dir": {DocID: "not-a-dir", Type: consts.FileType},
	})
	for _, tt := range []struct {
		dirID, want string
		wantErr     error
	}{
		{dirID: consts.RootDirID, want: "/a.txt"},
		{dirID: consts.TrashDirID, want: vfs.TrashDirName + "/a.txt"},
		{dirID: "loaded", want: "/loaded/a.txt"},
		{dirID: "missing", wantErr: vfs.ErrParentDoesNotExist},
		{dirID: "not-a-dir", wantErr: vfs.ErrParentDoesNotExist},
		{dirID: "fetched", want: "/fetched/a.txt"},
		{dirID: "unknown", wantErr: vfs.ErrParentDoesNotExist},
	} {
		t.Run(tt.dirID, func(t *testing.T) {
			got, err := fp.FilePath(&vfs.FileDoc{DocName: "a.txt", DirID: tt.dirID})
			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, tt.want, got)
		})
	}
	// Only the parents absent from the map are fetched.
	assert.Equal(t, []string{"fetched", "unknown"}, idx.calls)
}

func TestDirOrFileDocsByIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("a couchdb is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	db := prefixer.NewPrefixer(0, "dir-or-file-docs.example.net", "dir-or-file-docs.example.net")
	require.NoError(t, couchdb.ResetDB(db, consts.Files))
	t.Cleanup(func() { _ = couchdb.DeleteDB(db, consts.Files) })

	dir := &vfs.DirDoc{DocID: "dir", Type: consts.DirType, DocName: "dir", DirID: consts.RootDirID, Fullpath: "/dir"}
	file := &vfs.FileDoc{DocID: "file", Type: consts.FileType, DocName: "file.txt", DirID: "dir"}
	deleted := &vfs.FileDoc{DocID: "deleted", Type: consts.FileType, DocName: "deleted.txt", DirID: "dir"}
	for _, doc := range []couchdb.Doc{dir, file, deleted} {
		require.NoError(t, couchdb.CreateNamedDocWithDB(db, doc))
	}
	require.NoError(t, couchdb.DeleteDoc(db, deleted))

	docs, err := vfs.DirOrFileDocsByIDs(db, []string{"dir", "file", "missing", "deleted"})
	require.NoError(t, err)
	assert.Len(t, docs, 2)
	require.Contains(t, docs, "dir")
	assert.Equal(t, consts.DirType, docs["dir"].Type)
	assert.Equal(t, "/dir", docs["dir"].Fullpath)
	require.Contains(t, docs, "file")
	assert.Equal(t, consts.FileType, docs["file"].Type)
	assert.Equal(t, "dir", docs["file"].DirID)

	docs, err = vfs.DirOrFileDocsByIDs(db, nil)
	require.NoError(t, err)
	assert.Empty(t, docs)
}
