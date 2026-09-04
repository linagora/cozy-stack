package rag

import (
	"testing"

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
