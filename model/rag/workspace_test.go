package rag

import (
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/stretchr/testify/assert"
)

func TestKnowledgeBaseDirID(t *testing.T) {
	t.Run("returns the dirId of the io.cozy.files entry", func(t *testing.T) {
		a := &chatAssistant{KnowledgeBase: []knowledgeBaseEntry{
			{Doctype: "com.linagora.email", DirID: "nope"},
			{Doctype: "io.cozy.files", DirID: "folder-1"},
		}}
		assert.Equal(t, "folder-1", knowledgeBaseDirID(a, testLogger()))
	})
	t.Run("returns empty without knowledge base", func(t *testing.T) {
		assert.Equal(t, "", knowledgeBaseDirID(&chatAssistant{}, testLogger()))
		assert.Equal(t, "", knowledgeBaseDirID(nil, testLogger()))
	})
	t.Run("only the first files entry is used, extras are ignored", func(t *testing.T) {
		a := &chatAssistant{KnowledgeBase: []knowledgeBaseEntry{
			{Doctype: "io.cozy.files", DirID: "folder-1"},
			{Doctype: "io.cozy.files", DirID: "folder-2"},
		}}
		assert.Equal(t, "folder-1", knowledgeBaseDirID(a, testLogger()))
	})
}

func TestPruneMissingWorkspaces(t *testing.T) {
	folders := map[string]string{
		"kb1": "/Perso/HR",
		"kb2": "/Projects",
	}
	// kb2's workspace does not exist in openRAG: it must never be desired,
	// so it is pruned from the folder set.
	pruneMissingWorkspaces(folders, []string{"kb1", "ws-foreign"})
	assert.Equal(t, map[string]string{"kb1": "/Perso/HR"}, folders)

	pruneMissingWorkspaces(folders, nil)
	assert.Empty(t, folders)
}

func TestDiffMembership(t *testing.T) {
	toAdd, toRemove := diffMembership([]string{"a", "b"}, []string{"b", "c"})
	assert.Equal(t, []string{"a"}, toAdd)
	assert.Equal(t, []string{"c"}, toRemove)

	toAdd, toRemove = diffMembership(nil, nil)
	assert.Empty(t, toAdd)
	assert.Empty(t, toRemove)
}

func TestDesiredWorkspaces(t *testing.T) {
	kb := kbContext{
		// dirID -> folder path; loadKBContext only keeps folders whose
		// workspace already exists in openRAG.
		folders: map[string]string{
			"kb1": "/Perso/HR",
			"kb2": "/Projects",
		},
	}
	assert.Equal(t, []string{"kb1"}, kb.desiredWorkspaces("/Perso/HR/contracts"))
	assert.Equal(t, []string{"kb1"}, kb.desiredWorkspaces("/Perso/HR"))
	assert.Empty(t, kb.desiredWorkspaces("/Perso/HRX")) // no false prefix match
	assert.Empty(t, kb.desiredWorkspaces("/Elsewhere"))
	assert.Empty(t, kb.desiredWorkspaces(vfs.TrashDirName+"/Perso/HR")) // trashed content
}
