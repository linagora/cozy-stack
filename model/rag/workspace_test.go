package rag

import (
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/assert"
)

func TestKnowledgeBaseDirID(t *testing.T) {
	t.Run("returns the dirId of the io.cozy.files entry", func(t *testing.T) {
		a := &chatAssistant{KnowledgeBase: []knowledgeBaseEntry{
			{Doctype: "com.linagora.email", DirID: "nope"},
			{Doctype: "io.cozy.files", DirID: "folder-1"},
		}}
		assert.Equal(t, "folder-1", a.knowledgeBaseDirID(TestingLogger()))
	})
	t.Run("returns empty without knowledge base", func(t *testing.T) {
		assert.Equal(t, "", (&chatAssistant{}).knowledgeBaseDirID(TestingLogger()))
		assert.Equal(t, "", (*chatAssistant)(nil).knowledgeBaseDirID(TestingLogger()))
	})
	t.Run("only the first files entry is used, extras are ignored", func(t *testing.T) {
		a := &chatAssistant{KnowledgeBase: []knowledgeBaseEntry{
			{Doctype: "io.cozy.files", DirID: "folder-1"},
			{Doctype: "io.cozy.files", DirID: "folder-2"},
		}}
		assert.Equal(t, "folder-1", a.knowledgeBaseDirID(TestingLogger()))
	})
}

func TestWorkspaceIDForDir(t *testing.T) {
	t.Run("every well-known folder id becomes a valid workspace id", func(t *testing.T) {
		// openRAG only accepts ^[A-Za-z0-9_-]+$ and answers 422 otherwise;
		// the well-known folder ids are the only ones with dots.
		assert.Equal(t, map[string]string{
			consts.RootDirID:           "io-cozy-files-root-dir",
			consts.TrashDirID:          "io-cozy-files-trash-dir",
			consts.SharedWithMeDirID:   "io-cozy-files-shared-with-me-dir",
			consts.NoLongerSharedDirID: "io-cozy-files-no-longer-shared-dir",
			consts.SharedDrivesDirID:   "io-cozy-files-shared-drives-dir",
		}, wellKnownWorkspaceIDs)
		for dirID, workspaceID := range wellKnownWorkspaceIDs {
			assert.Regexp(t, `^[A-Za-z0-9_-]+$`, workspaceID)
			assert.Equal(t, strings.ReplaceAll(dirID, ".", "-"), workspaceID)
			assert.Equal(t, workspaceID, workspaceIDForDir(dirID))
		}
		assert.Len(t, wellKnownDirIDs, len(wellKnownWorkspaceIDs), "the ids are mapped one to one")
	})
	t.Run("a folder id is its own workspace id", func(t *testing.T) {
		assert.Equal(t, "2ce11217a0efbe69b18e5d81de178422", workspaceIDForDir("2ce11217a0efbe69b18e5d81de178422"))
	})
	t.Run("round trip", func(t *testing.T) {
		dirIDs := []string{"2ce11217a0efbe69b18e5d81de178422"}
		for dirID := range wellKnownWorkspaceIDs {
			dirIDs = append(dirIDs, dirID)
		}
		for _, dirID := range dirIDs {
			assert.Equal(t, dirID, dirIDForWorkspace(workspaceIDForDir(dirID)))
		}
		workspaceIDs := []string{"2ce11217a0efbe69b18e5d81de178422"}
		for _, workspaceID := range wellKnownWorkspaceIDs {
			workspaceIDs = append(workspaceIDs, workspaceID)
		}
		for _, ws := range workspaceIDs {
			assert.Equal(t, ws, workspaceIDForDir(dirIDForWorkspace(ws)))
		}
	})
	t.Run("slices keep their order", func(t *testing.T) {
		assert.Equal(t, []string{"abc", rootWorkspaceID}, workspaceIDsForDirs([]string{"abc", consts.RootDirID}))
		assert.Equal(t, []string{"abc", consts.RootDirID}, dirIDsForWorkspaces([]string{"abc", rootWorkspaceID}))
		assert.Empty(t, workspaceIDsForDirs(nil))
		assert.Empty(t, dirIDsForWorkspaces(nil))
	})
}
