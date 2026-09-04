package rag

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// formFields reads back the multipart body the RAG server received. ReadForm
// keeps the file part out of Value, so only the fields are returned.
func formFields(t *testing.T, contentType string, body []byte) url.Values {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)

	form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(int64(len(body)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = form.RemoveAll() })
	return form.Value
}

func TestUploadToRAG(t *testing.T) {
	upload := func(t *testing.T, up ragUpload) (RecordedRequest, string) {
		t.Helper()
		var contentType string
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			contentType = req.Header.Get("Content-Type")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		})
		up.Server = server
		up.Domain = "alice.example.net"
		res, err := uploadToRAG(up, strings.NewReader("hello"))
		require.NoError(t, err)
		defer res.Body.Close()
		requests := rec.All()
		require.Len(t, requests, 1)
		return requests[0], contentType
	}

	t.Run("the callback URL and the file revision reach the RAG server", func(t *testing.T) {
		req, contentType := upload(t, ragUpload{
			FileID:      "a1b2c3",
			Name:        "note.txt",
			MD5Sum:      "098f6bcd4621d373cade4e832627b4f6",
			Meta:        map[string]string{"doc_rev": "3-abc", "doctype": "io.cozy.files"},
			CallbackURL: "https://alice.example.net/ai/index/status",
		})

		fields := formFields(t, contentType, req.Body)
		assert.Equal(t, "https://alice.example.net/ai/index/status", fields.Get("callback_url"))

		var meta map[string]string
		require.NoError(t, json.Unmarshal([]byte(fields.Get("metadata")), &meta))
		assert.Equal(t, "3-abc", meta["doc_rev"])
		assert.Equal(t, "io.cozy.files", meta["doctype"])
	})

	t.Run("a known file is sent with a PUT, a new one with a POST", func(t *testing.T) {
		req, _ := upload(t, ragUpload{FileID: "a1b2c3", Name: "note.txt", IsNew: false})
		assert.Equal(t, http.MethodPut, req.Method)
		assert.Equal(t, "/indexer/partition/alice.example.net/file/a1b2c3", req.Path)

		req, _ = upload(t, ragUpload{FileID: "a1b2c3", Name: "note.txt", IsNew: true})
		assert.Equal(t, http.MethodPost, req.Method)
	})

	t.Run("workspaces are only sent when the file belongs to some", func(t *testing.T) {
		req, contentType := upload(t, ragUpload{FileID: "a1b2c3", Name: "note.txt"})
		assert.NotContains(t, formFields(t, contentType, req.Body), "workspace_ids")

		req, contentType = upload(t, ragUpload{FileID: "a1b2c3", Name: "note.txt", Workspaces: `["ws1"]`})
		assert.Equal(t, `["ws1"]`, formFields(t, contentType, req.Body).Get("workspace_ids"))
	})
}

func TestDeleteFromRAGHTTP(t *testing.T) {
	t.Run("the file is deleted from its partition", func(t *testing.T) {
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		require.NoError(t, deleteFromRAGHTTP(server, "alice.example.net", "a1b2c3"))

		requests := rec.All()
		require.Len(t, requests, 1)
		assert.Equal(t, http.MethodDelete, requests[0].Method)
		assert.Equal(t, "/indexer/partition/alice.example.net/file/a1b2c3", requests[0].Path)
	})

	t.Run("a file the RAG server does not know is not an error", func(t *testing.T) {
		server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		assert.NoError(t, deleteFromRAGHTTP(server, "alice.example.net", "a1b2c3"))
	})

	t.Run("a server error is reported so the job is retried", func(t *testing.T) {
		server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		assert.Error(t, deleteFromRAGHTTP(server, "alice.example.net", "a1b2c3"))
	})
}

func TestIndexedMD5Sum(t *testing.T) {
	t.Run("the md5sum the RAG server holds is returned", func(t *testing.T) {
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"metadata":{"md5sum":"098f6bcd4621d373cade4e832627b4f6"}}`))
		})
		md5sum, known, err := indexedMD5Sum(server, "alice.example.net", "a1b2c3")
		require.NoError(t, err)
		assert.True(t, known)
		assert.Equal(t, "098f6bcd4621d373cade4e832627b4f6", md5sum)
		assert.Equal(t, "/partition/alice.example.net/file/a1b2c3", rec.All()[0].Path)
	})

	t.Run("an unknown file is reported as such rather than as an error", func(t *testing.T) {
		server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		md5sum, known, err := indexedMD5Sum(server, "alice.example.net", "a1b2c3")
		require.NoError(t, err)
		assert.False(t, known)
		assert.Empty(t, md5sum)
	})

	t.Run("a file with no metadata is known with no md5sum", func(t *testing.T) {
		server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		})
		md5sum, known, err := indexedMD5Sum(server, "alice.example.net", "a1b2c3")
		require.NoError(t, err)
		assert.True(t, known)
		assert.Empty(t, md5sum)
	})

	t.Run("a server error is reported", func(t *testing.T) {
		server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		_, _, err := indexedMD5Sum(server, "alice.example.net", "a1b2c3")
		assert.Error(t, err)
	})
}
