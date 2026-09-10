package rag

import (
	"testing"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/assert"
)

func TestUnderFolder(t *testing.T) {
	assert.True(t, underFolder("/Docs/KB", "/Docs/KB"))
	assert.True(t, underFolder("/Docs/KB/sub", "/Docs/KB"))
	assert.False(t, underFolder("/Docs/KB2", "/Docs/KB"))
	assert.False(t, underFolder("/Docs", "/Docs/KB"))
	assert.False(t, underFolder("/.cozy_trash/Docs/KB/sub", "/Docs/KB"))
	assert.False(t, underFolder("/.cozy_trash/KB", "/.cozy_trash/KB"), "a trashed folder claims nothing")

	assert.True(t, underFolder("/", "/"), "root claims files at the root")
	assert.True(t, underFolder("/Docs/KB", "/"), "root claims everything")
	assert.False(t, underFolder("/.cozy_trash/x", "/"), "root does not claim the trash")
}

func TestDesiredFor(t *testing.T) {
	s := &scopes{folders: map[string]string{
		"outer": "/Outer",
		"inner": "/Outer/Inner",
		"other": "/Other",
	}}
	assert.Equal(t, []string{"inner", "outer"}, s.desiredFor("/Outer/Inner/deep"))
	assert.Equal(t, []string{"outer"}, s.desiredFor("/Outer"))
	assert.Equal(t, []string{"other"}, s.desiredFor("/Other/x"))
	assert.Empty(t, s.desiredFor("/Elsewhere"))
	assert.Empty(t, s.desiredFor("/.cozy_trash/Outer/Inner"))

	root := &scopes{folders: map[string]string{consts.RootDirID: "/", "inner": "/Outer/Inner"}}
	assert.Equal(t, []string{"inner", consts.RootDirID}, root.desiredFor("/Outer/Inner"))
	assert.Equal(t, []string{consts.RootDirID}, root.desiredFor("/Elsewhere"))
	assert.Empty(t, (&scopes{folders: map[string]string{}}).desiredFor("/x"))
}

func TestDiffWorkspaces(t *testing.T) {
	folders := map[string]string{"a": "/A", "b": "/B"}
	toCreate, toRemove := diffWorkspaces(folders, []string{"b", "stale", "zzz"})
	assert.Equal(t, []string{"a"}, toCreate)
	assert.Equal(t, []string{"stale", "zzz"}, toRemove)

	toCreate, toRemove = diffWorkspaces(map[string]string{}, []string{"x"})
	assert.Empty(t, toCreate)
	assert.Equal(t, []string{"x"}, toRemove)
}
