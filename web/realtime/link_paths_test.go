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

	t.Run("OtherDoctype", func(t *testing.T) {
		doc := &realtime.JSONDoc{Type: "io.cozy.todos", M: map[string]interface{}{
			"_id": "todo", "path": "/Private/Other", "restore_path": "/Private/Other",
		}}
		res := l.rewrite(doc).(*realtime.JSONDoc)
		assert.Equal(t, "/Private/Other", res.M["path"])
		assert.Equal(t, "/Private/Other", res.M["restore_path"])
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

	t.Run("NestedRootMoved", func(t *testing.T) {
		child, err := vfs.MkdirAll(fs, "/Private/Meetings/Child")
		require.NoError(t, err)
		meetings, err := fs.DirByPath("/Private/Meetings")
		require.NoError(t, err)
		nested := &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{permission.Rule{
			Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{meetings.ID(), child.ID()},
		}}}
		n, err := newLinkPaths(inst, nested)
		require.NoError(t, err)

		name := "Reviewed"
		child, err = vfs.ModifyDirMetadata(fs, child, &vfs.DocPatch{Name: &name})
		require.NoError(t, err)
		assert.Equal(t, "/Meetings/Reviewed", n.rewrite(child).(*vfs.DirDoc).Fullpath)

		child, err = vfs.TrashDir(fs, child)
		require.NoError(t, err)
		require.Equal(t, "/.cozy_trash/Reviewed", child.Fullpath)
		assert.Equal(t, "/.cozy_trash/Reviewed", n.rewrite(child).(*vfs.DirDoc).Fullpath)
	})

	readLink := func(t *testing.T, ids ...string) *linkPaths {
		t.Helper()
		l, err := newLinkPaths(inst, &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{permission.Rule{
			Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: ids,
		}}})
		require.NoError(t, err)
		return l
	}

	t.Run("AncestorTrashed", func(t *testing.T) {
		reports, err := vfs.MkdirAll(fs, "/Archive/Old/Reports")
		require.NoError(t, err)
		a := readLink(t, reports.ID())
		old, err := fs.DirByPath("/Archive/Old")
		require.NoError(t, err)
		old, err = vfs.TrashDir(fs, old)
		require.NoError(t, err)
		require.Equal(t, "/.cozy_trash/Old", old.Fullpath)

		// Only the event of a document under the root comes
		sub := &vfs.DirDoc{DocID: "sub", Fullpath: "/.cozy_trash/Old/Reports/Sub"}
		assert.Equal(t, "/.cozy_trash/Reports/Sub", a.rewrite(sub).(*vfs.DirDoc).Fullpath)
		left := &vfs.DirDoc{DocID: "left", Fullpath: "/.cozy_trash/Other/Left"}
		assert.Empty(t, a.rewrite(left).(*vfs.DirDoc).Fullpath)
	})

	t.Run("InnerRootRenamed", func(t *testing.T) {
		draft, err := vfs.MkdirAll(fs, "/Team/Docs/Draft")
		require.NoError(t, err)
		team, err := fs.DirByPath("/Team")
		require.NoError(t, err)
		inner, err := fs.DirByPath("/Team/Docs")
		require.NoError(t, err)
		o := readLink(t, inner.ID(), team.ID())

		name := "Renamed"
		_, err = vfs.ModifyDirMetadata(fs, inner, &vfs.DocPatch{Name: &name})
		require.NoError(t, err)
		draft, err = fs.DirByID(draft.ID())
		require.NoError(t, err)
		// Only the event of Draft comes, as for a subscription to it alone
		assert.Equal(t, "/Renamed/Draft", o.rewrite(draft).(*vfs.DirDoc).Fullpath)
	})

	t.Run("RootNameReused", func(t *testing.T) {
		draft, err := vfs.MkdirAll(fs, "/Office/Planning/Draft")
		require.NoError(t, err)
		inner, err := vfs.MkdirAll(fs, "/Office/Docs")
		require.NoError(t, err)
		office, err := fs.DirByPath("/Office")
		require.NoError(t, err)
		planning, err := fs.DirByPath("/Office/Planning")
		require.NoError(t, err)
		r := readLink(t, inner.ID(), office.ID())

		// No event comes for Docs, as the subscription watches Draft only
		archive, reused := "Archive", "Docs"
		_, err = vfs.ModifyDirMetadata(fs, inner, &vfs.DocPatch{Name: &archive})
		require.NoError(t, err)
		_, err = vfs.ModifyDirMetadata(fs, planning, &vfs.DocPatch{Name: &reused})
		require.NoError(t, err)
		draft, err = fs.DirByID(draft.ID())
		require.NoError(t, err)
		assert.Equal(t, "/Office/Docs/Draft", r.rewrite(draft).(*vfs.DirDoc).Fullpath)
	})
}
