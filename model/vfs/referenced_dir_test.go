package vfs_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type referencedDirFS struct {
	vfs.VFS
	dir                  *vfs.DirDoc
	createErr, updateErr error
}

func (fs *referencedDirFS) DirByPath(path string) (*vfs.DirDoc, error) {
	if path != fs.dir.Fullpath {
		return nil, os.ErrNotExist
	}
	return fs.dir, nil
}
func (fs *referencedDirFS) UpdateDirDoc(_, _ *vfs.DirDoc) error { return fs.updateErr }
func (fs *referencedDirFS) CreateDir(dir *vfs.DirDoc) error {
	if fs.createErr == nil {
		dir.DocID = "created"
	}
	return fs.createErr
}

func TestEnsureReferencedDir(t *testing.T) {
	failure := errors.New("storage failure")
	ref := couchdb.DocReference{Type: consts.Apps, ID: consts.Apps + "/mail"}
	db := prefixer.NewPrefixer(0, "referenced-dir-test.local", "referenced-dir-test")
	downloads := &vfs.DirDoc{DocID: "downloads", Type: consts.DirType, DirID: consts.RootDirID, Fullpath: "/Downloads"}
	for _, tt := range []struct {
		name, wantID                  string
		docs                          []vfs.DirDoc
		parent                        *vfs.DirDoc
		createErr, updateErr, wantErr error
	}{
		{name: "create", wantID: "created"},
		{name: "reuse existing path", createErr: os.ErrExist, wantID: "existing"},
		{name: "reuse existing path in parent", createErr: os.ErrExist, wantID: "existing", parent: downloads},
		{name: "creation failure", createErr: failure, wantErr: failure},
		{name: "reference update failure", createErr: os.ErrExist, updateErr: failure, wantErr: failure},
		{name: "existing reference", wantID: "existing", docs: []vfs.DirDoc{
			{DocID: "existing", Type: consts.DirType, Fullpath: "/Renamed/Mail"},
		}},
		{name: "leave trash untouched", wantID: "created", docs: []vfs.DirDoc{
			{DocID: "trashed", Type: consts.DirType, Fullpath: vfs.TrashDirName + "/Mail"},
		}},
		{name: "only count a directory in its parent", wantID: "inside", parent: downloads, docs: []vfs.DirDoc{
			{DocID: "outside", Type: consts.DirType, DirID: consts.RootDirID, Fullpath: "/Mail"},
			{DocID: "inside", Type: consts.DirType, DirID: downloads.DocID, Fullpath: "/Downloads/Mail"},
		}},
		{name: "cleanup failure after finding a match", parent: downloads, updateErr: failure, wantErr: failure, docs: []vfs.DirDoc{
			{DocID: "inside", Type: consts.DirType, DirID: downloads.DocID},
			{DocID: "outside", Type: consts.DirType, DirID: consts.RootDirID, ReferencedBy: []couchdb.DocReference{ref}},
		}},
		{name: "skip a referenced file", wantID: "inside", parent: downloads, docs: []vfs.DirDoc{
			{DocID: "file", Type: consts.FileType, DirID: downloads.DocID},
			{DocID: "inside", Type: consts.DirType, DirID: downloads.DocID, Fullpath: "/Downloads/Mail"},
		}},
		{name: "create in the parent", wantID: "created", parent: downloads, docs: []vfs.DirDoc{
			{DocID: "outside", Type: consts.DirType, DirID: consts.RootDirID, Fullpath: "/Mail"},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := make([]map[string]interface{}, 0, len(tt.docs))
			for _, doc := range tt.docs {
				rows = append(rows, map[string]interface{}{"id": doc.ID(), "doc": doc})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "true", r.URL.Query().Get("include_docs"))
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"rows": rows}))
			}))
			defer server.Close()
			t.Setenv("COZY_COUCHDB_URL", server.URL)
			config.UseTestFile(t)
			fs := &referencedDirFS{
				dir:       &vfs.DirDoc{DocID: "existing", Type: consts.DirType, Fullpath: "/Mail"},
				createErr: tt.createErr, updateErr: tt.updateErr,
			}
			if tt.parent != nil {
				fs.dir.DirID, fs.dir.Fullpath = downloads.DocID, "/Downloads/Mail"
			}
			dir, err := vfs.EnsureReferencedDir(db, fs, ref, tt.parent, "Mail", "")
			require.ErrorIs(t, err, tt.wantErr)
			if tt.wantErr == nil {
				require.NotNil(t, dir)
				assert.Equal(t, tt.wantID, dir.ID())
				if len(tt.docs) == 0 || tt.wantID == "created" {
					assert.Contains(t, dir.ReferencedBy, ref)
				}
				if tt.parent != nil {
					assert.Equal(t, tt.parent.DocID, dir.DirID)
				}
			}
		})
	}
}

func TestRenameKeepsUnrelatedReferences(t *testing.T) {
	notes := couchdb.DocReference{Type: consts.Apps, ID: consts.Apps + "/notes"}
	unknown := couchdb.DocReference{Type: consts.Apps, ID: consts.Apps + "/other"}
	mail := couchdb.DocReference{Type: consts.Apps, ID: consts.Apps + "/mail"}
	album := couchdb.DocReference{Type: consts.PhotosAlbums, ID: mail.ID}
	dir, err := vfs.NewDirDocWithPath("Mail", "downloads", "/Downloads", nil)
	require.NoError(t, err)
	dir.AddReferencedBy(notes, mail, unknown, album)

	name := "Renamed"
	renamed, err := vfs.ModifyDirMetadata(&referencedDirFS{}, dir, &vfs.DocPatch{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, []couchdb.DocReference{notes, unknown, album}, renamed.ReferencedBy)
}
