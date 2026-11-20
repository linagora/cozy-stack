package realtime

import (
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinkPathsRewrite(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	docs, err := vfs.MkdirAll(fs, "/Private/Clients/Docs")
	require.NoError(t, err)
	perm := &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{permission.Rule{
		Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{docs.ID()},
	}}}
	l, err := newLinkPaths(inst, perm)
	require.NoError(t, err)

	t.Run("DirDoc", func(t *testing.T) {
		dir := &vfs.DirDoc{DocID: "sub", Fullpath: "/Private/Clients/Docs/Sub", RestorePath: "/Private/Clients/Docs"}
		res := l.rewrite(dir).(*vfs.DirDoc)
		assert.Equal(t, "/Docs/Sub", res.Fullpath)
		assert.Equal(t, "/Docs", res.RestorePath)
		assert.Equal(t, "/Private/Clients/Docs/Sub", dir.Fullpath)
	})

	t.Run("FileDoc", func(t *testing.T) {
		file := &vfs.FileDoc{DocID: "f", RestorePath: "/Private/Other"}
		assert.Empty(t, l.rewrite(file).(*vfs.FileDoc).RestorePath)
	})

	t.Run("JSONDoc", func(t *testing.T) {
		doc := &realtime.JSONDoc{Type: consts.Files, M: map[string]interface{}{
			"_id": "sub", "path": "/Private/Clients/Docs/Sub", "restore_path": "/Private/Other",
		}}
		res := l.rewrite(doc).(*realtime.JSONDoc)
		assert.Equal(t, "/Docs/Sub", res.M["path"])
		assert.NotContains(t, res.M, "restore_path")
		assert.Equal(t, "/Private/Other", doc.M["restore_path"])
	})

	t.Run("TargetGone", func(t *testing.T) {
		gone := &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{permission.Rule{
			Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{"no-such-file"},
		}}}
		g, err := newLinkPaths(inst, gone)
		require.NoError(t, err)
		assert.True(t, g.restricted())
		dir := &vfs.DirDoc{DocID: "sub", Fullpath: "/Private/Clients/Docs/Sub"}
		assert.Empty(t, g.rewrite(dir).(*vfs.DirDoc).Fullpath)
	})

	t.Run("WholeDoctype", func(t *testing.T) {
		whole := &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{permission.Rule{
			Type: consts.Files, Verbs: permission.Verbs(permission.GET),
		}}}
		w, err := newLinkPaths(inst, whole)
		require.NoError(t, err)
		assert.False(t, w.restricted())
	})

	t.Run("AncestorRenamed", func(t *testing.T) {
		clients, err := fs.DirByPath("/Private/Clients")
		require.NoError(t, err)
		name := "Renamed"
		_, err = vfs.ModifyDirMetadata(fs, clients, &vfs.DocPatch{Name: &name})
		require.NoError(t, err)

		root := &vfs.DirDoc{DocID: docs.ID(), Fullpath: "/Private/Renamed/Docs"}
		assert.Equal(t, "/Docs", l.rewrite(root).(*vfs.DirDoc).Fullpath)
		sub := &vfs.DirDoc{DocID: "sub", Fullpath: "/Private/Renamed/Docs/Sub"}
		assert.Equal(t, "/Docs/Sub", l.rewrite(sub).(*vfs.DirDoc).Fullpath)
	})
}
