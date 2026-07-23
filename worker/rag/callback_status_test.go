package rag

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	modelrag "github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestWorkerIndexStatus(t *testing.T) {
	config.UseTestFile(t)
	setup := testutils.NewSetup(t, "rag_status_test")
	inst := setup.GetTestInstance(&lifecycle.Options{})

	runWorker := func(t *testing.T, payload map[string]interface{}) error {
		t.Helper()
		data, err := json.Marshal(payload)
		require.NoError(t, err)
		j := &job.Job{
			Domain:     inst.Domain,
			WorkerType: "rag-index-status",
			Payload:    job.Payload(data),
		}
		ctx, cancel := job.NewTaskContext("test", j, inst)
		defer cancel()
		return WorkerIndexStatus(ctx)
	}

	t.Run("success status sets Status=success and Indexed=true", func(t *testing.T) {
		fs := inst.VFS()
		doc := createStatusTestFile(t, fs, "rag-success.txt")
		defer destroyStatusTestFile(t, fs, doc)

		ts := time.Now().UTC().Truncate(time.Second)
		err := runWorker(t, map[string]interface{}{
			"partition": inst.Domain,
			"file_id":   doc.DocID,
			"status":    "success",
			"timestamp": ts.Format(time.RFC3339Nano),
		})
		require.NoError(t, err)

		updated, err := fs.FileByID(doc.DocID)
		require.NoError(t, err)
		require.NotNil(t, updated.CozyMetadata.RAG)
		require.True(t, updated.CozyMetadata.RAG.Indexed)
		require.Equal(t, modelrag.RAGStatusSuccess, updated.CozyMetadata.RAG.Status)
		require.NotNil(t, updated.CozyMetadata.RAG.LastSuccessDate)
		require.Nil(t, updated.CozyMetadata.RAG.LastErrorDate)
	})

	t.Run("error status sets Status=error and preserves Indexed", func(t *testing.T) {
		fs := inst.VFS()
		doc := createStatusTestFile(t, fs, "rag-error.txt")
		defer destroyStatusTestFile(t, fs, doc)

		ts := time.Now().UTC().Truncate(time.Second)
		err := runWorker(t, map[string]interface{}{
			"partition": inst.Domain,
			"file_id":   doc.DocID,
			"status":    "error",
			"timestamp": ts.Format(time.RFC3339Nano),
		})
		require.NoError(t, err)

		updated, err := fs.FileByID(doc.DocID)
		require.NoError(t, err)
		require.NotNil(t, updated.CozyMetadata.RAG)
		require.False(t, updated.CozyMetadata.RAG.Indexed)
		require.Equal(t, modelrag.RAGStatusError, updated.CozyMetadata.RAG.Status)
		require.Nil(t, updated.CozyMetadata.RAG.LastSuccessDate)
		require.NotNil(t, updated.CozyMetadata.RAG.LastErrorDate)
	})

	t.Run("notsupported status sets Status=notsupported without touching Indexed or dates", func(t *testing.T) {
		fs := inst.VFS()
		doc := createStatusTestFile(t, fs, "rag-notsupported.txt")
		defer destroyStatusTestFile(t, fs, doc)

		err := runWorker(t, map[string]interface{}{
			"partition": inst.Domain,
			"file_id":   doc.DocID,
			"status":    "notsupported",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		})
		require.NoError(t, err)

		updated, err := fs.FileByID(doc.DocID)
		require.NoError(t, err)
		require.NotNil(t, updated.CozyMetadata.RAG)
		require.Equal(t, modelrag.RAGStatusNotSupported, updated.CozyMetadata.RAG.Status)
		require.False(t, updated.CozyMetadata.RAG.Indexed)
		require.Nil(t, updated.CozyMetadata.RAG.LastSuccessDate)
		require.Nil(t, updated.CozyMetadata.RAG.LastErrorDate)
	})

	t.Run("unknown status returns error", func(t *testing.T) {
		err := runWorker(t, map[string]interface{}{
			"partition": inst.Domain,
			"file_id":   "any-file-id",
			"status":    "weird",
		})
		require.Error(t, err)
	})

	t.Run("missing timestamp falls back to now without error", func(t *testing.T) {
		fs := inst.VFS()
		doc := createStatusTestFile(t, fs, "rag-no-ts.txt")
		defer destroyStatusTestFile(t, fs, doc)

		err := runWorker(t, map[string]interface{}{
			"partition": inst.Domain,
			"file_id":   doc.DocID,
			"status":    "success",
		})
		require.NoError(t, err)

		updated, err := fs.FileByID(doc.DocID)
		require.NoError(t, err)
		require.NotNil(t, updated.CozyMetadata.RAG.LastSuccessDate)
	})

	t.Run("malformed timestamp falls back to now without error", func(t *testing.T) {
		fs := inst.VFS()
		doc := createStatusTestFile(t, fs, "rag-bad-ts.txt")
		defer destroyStatusTestFile(t, fs, doc)

		err := runWorker(t, map[string]interface{}{
			"partition": inst.Domain,
			"file_id":   doc.DocID,
			"status":    "success",
			"timestamp": "not-a-date",
		})
		require.NoError(t, err)

		updated, err := fs.FileByID(doc.DocID)
		require.NoError(t, err)
		require.NotNil(t, updated.CozyMetadata.RAG.LastSuccessDate)
	})

	t.Run("non-existent file_id returns nil error", func(t *testing.T) {
		err := runWorker(t, map[string]interface{}{
			"partition": inst.Domain,
			"file_id":   "non-existent-file-id",
			"status":    "success",
		})
		require.NoError(t, err)
	})
}

func createStatusTestFile(t *testing.T, fs vfs.VFS, name string) *vfs.FileDoc {
	t.Helper()
	parent, err := fs.DirByPath("/")
	require.NoError(t, err)
	doc, err := vfs.NewFileDoc(name, parent.DocID, 4, nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := fs.CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = f.Write([]byte("test"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	updated, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	return updated
}

func destroyStatusTestFile(t *testing.T, fs vfs.VFS, doc *vfs.FileDoc) {
	t.Helper()
	_ = fs.DestroyFile(doc)
}
