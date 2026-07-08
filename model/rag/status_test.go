package rag

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/stretchr/testify/assert"
)

func TestApplyRAGStatus(t *testing.T) {
	t1 := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)

	t.Run("success sets Indexed=true and LastSuccessDate", func(t *testing.T) {
		rag := &vfs.RAGMetadata{}
		applyRAGStatus(rag, RAGStatusSuccess, t1)
		assert.True(t, rag.Indexed)
		assert.Equal(t, RAGStatusSuccess, rag.Status)
		assert.Equal(t, t1, *rag.LastSuccessDate)
		assert.Nil(t, rag.LastErrorDate)
	})

	t.Run("error without prior success keeps Indexed=false", func(t *testing.T) {
		rag := &vfs.RAGMetadata{}
		applyRAGStatus(rag, RAGStatusError, t1)
		assert.False(t, rag.Indexed)
		assert.Equal(t, RAGStatusError, rag.Status)
		assert.Equal(t, t1, *rag.LastErrorDate)
		assert.Nil(t, rag.LastSuccessDate)
	})

	t.Run("error after success preserves Indexed=true", func(t *testing.T) {
		rag := &vfs.RAGMetadata{Indexed: true}
		applyRAGStatus(rag, RAGStatusError, t1)
		assert.True(t, rag.Indexed)
		assert.Equal(t, RAGStatusError, rag.Status)
		assert.Equal(t, t1, *rag.LastErrorDate)
	})
}

func TestIsOutdated(t *testing.T) {
	file := &vfs.FileDoc{MD5Sum: []byte{0x01, 0x02, 0x03}} // hex "010203"

	assert.False(t, isOutdated(file, "010203"))  // matches current content
	assert.True(t, isOutdated(file, "deadbeef")) // an older version
	assert.False(t, isOutdated(file, ""))        // no md5 => treated as current
}

func TestIsNewerThan(t *testing.T) {
	t1 := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	t.Run("newer than the last recorded date applies", func(t *testing.T) {
		rag := &vfs.RAGMetadata{LastSuccessDate: &t1}
		assert.True(t, isNewerThan(t2, rag))
		assert.False(t, isNewerThan(t1, rag))
	})

	t.Run("compares against the most recent of both dates", func(t *testing.T) {
		rag := &vfs.RAGMetadata{LastSuccessDate: &t1, LastErrorDate: &t2}
		assert.False(t, isNewerThan(t1.Add(30*time.Minute), rag)) // older than t2
		assert.True(t, isNewerThan(t2.Add(time.Minute), rag))
	})
}
