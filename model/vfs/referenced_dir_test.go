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

func (fs *referencedDirFS) DirByID(string) (*vfs.DirDoc, error)   { return fs.dir, nil }
func (fs *referencedDirFS) DirByPath(string) (*vfs.DirDoc, error) { return fs.dir, nil }
func (fs *referencedDirFS) UpdateDirDoc(_, _ *vfs.DirDoc) error   { return fs.updateErr }
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
	for _, tt := range []struct {
		name, wantID                  string
		docs                          []vfs.DirDoc
		createErr, updateErr, wantErr error
	}{
		{name: "create", wantID: "created"},
		{name: "reuse existing path", createErr: os.ErrExist, wantID: "existing"},
		{name: "creation failure", createErr: failure, wantErr: failure},
		{name: "reference update failure", createErr: os.ErrExist, updateErr: failure, wantErr: failure},
		{name: "existing reference", wantID: "existing", docs: []vfs.DirDoc{
			{DocID: "existing", Type: consts.DirType, Fullpath: "/Renamed/Mail"},
		}},
		{name: "leave trash untouched", wantID: "created", docs: []vfs.DirDoc{
			{DocID: "trashed", Type: consts.DirType, Fullpath: vfs.TrashDirName + "/Mail"},
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
			if len(tt.docs) > 0 {
				fs.dir = &tt.docs[0]
			}
			dir, err := vfs.EnsureReferencedDir(db, fs, ref, "Mail", "")
			require.ErrorIs(t, err, tt.wantErr)
			if tt.wantErr == nil {
				require.NotNil(t, dir)
				assert.Equal(t, tt.wantID, dir.ID())
				if len(tt.docs) == 0 {
					assert.Contains(t, dir.ReferencedBy, ref)
				}
			}
		})
	}
}
