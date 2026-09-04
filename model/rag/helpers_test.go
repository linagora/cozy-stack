// model/rag/helpers_test.go
package rag

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/stretchr/testify/require"
)

type recordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *requestRecorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.mu.Lock()
	r.requests = append(r.requests, recordedRequest{Method: req.Method, Path: req.URL.Path, Body: body})
	r.mu.Unlock()
}

func (r *requestRecorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func (r *requestRecorder) count(method, p string) int {
	n := 0
	for _, rr := range r.all() {
		if rr.Method == method && rr.Path == p {
			n++
		}
	}
	return n
}

func (r *requestRecorder) countPath(p string) int {
	n := 0
	for _, rr := range r.all() {
		if rr.Path == p {
			n++
		}
	}
	return n
}

func testLogger() logger.Logger {
	return logger.WithNamespace("rag-test")
}

func newRAGTestServer(t *testing.T, handler http.HandlerFunc) (config.RAGServer, *requestRecorder) {
	t.Helper()
	rec := &requestRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)
		handler(w, req)
	}))
	t.Cleanup(srv.Close)
	return config.RAGServer{URL: srv.URL, APIKey: "test-key"}, rec
}

func decodeFileIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var payload struct {
		FileIDs []string `json:"file_ids"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	return payload.FileIDs
}

// fakeFile is the state openRAG keeps for one indexed file.
type fakeFile struct {
	MD5        string
	Workspaces []string
}

// fakeOpenRAG is an in-memory openRAG implementing the routes the stack uses.
type fakeOpenRAG struct {
	t          *testing.T
	server     config.RAGServer
	rec        *requestRecorder
	mu         sync.Mutex
	files      map[string]*fakeFile
	workspaces map[string]bool
	// fail, when set, is consulted before every request: a non-zero status
	// is returned as-is without touching the state.
	fail func(method, path string) int
}

func newFakeOpenRAG(t *testing.T) *fakeOpenRAG {
	t.Helper()
	f := &fakeOpenRAG{t: t, files: map[string]*fakeFile{}, workspaces: map[string]bool{}}
	f.server, f.rec = newRAGTestServer(t, f.handle)
	return f
}

func (f *fakeOpenRAG) addFile(id, md5 string, workspaces ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[id] = &fakeFile{MD5: md5, Workspaces: append([]string{}, workspaces...)}
}

func (f *fakeOpenRAG) addWorkspace(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces[id] = true
}

func (f *fakeOpenRAG) file(id string) (fakeFile, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[id]
	if !ok {
		return fakeFile{}, false
	}
	return fakeFile{MD5: ff.MD5, Workspaces: append([]string{}, ff.Workspaces...)}, true
}

func (f *fakeOpenRAG) hasWorkspace(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workspaces[id]
}

func (f *fakeOpenRAG) fileIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.files))
	for id := range f.files {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handle routes (after url decoding of the path segments):
//
//	GET    /partition/{d}/                         → {"files":[{"link":".../partition/{d}/file/{id}"}]}
//	POST   /partition/{d}                          → 201
//	GET    /partition/{d}/file/{id}                → {"metadata":{"md5sum":..}} | 404
//	POST   /indexer/partition/{d}/file/{id}        → 200 (multipart, reads workspace_ids, md5sum from query)
//	PUT    /indexer/partition/{d}/file/{id}        → 200 (idem)
//	DELETE /indexer/partition/{d}/file/{id}        → 200 | 404
//	GET    /partition/{d}/workspaces/{ws}          → 200 | 404
//	POST   /partition/{d}/workspaces               → 201 | 409
//	DELETE /partition/{d}/workspaces/{ws}          → 200 | 404
//	GET    /partition/{d}/files/{id}/workspaces    → {"workspace_ids":[..]} | 404
//	POST   /partition/{d}/workspaces/{ws}/files    → 200 | 404 (unknown ws or any unknown id)
//	DELETE /partition/{d}/workspaces/{ws}/files/{id} → 200 | 404
func (f *fakeOpenRAG) handle(w http.ResponseWriter, req *http.Request) {
	if f.fail != nil {
		if status := f.fail(req.Method, req.URL.Path); status != 0 {
			writeJSON(w, status, map[string]string{"error": "injected"})
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	segs := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	trailingSlash := strings.HasSuffix(req.URL.Path, "/")
	switch {
	case req.Method == http.MethodGet && len(segs) == 2 && segs[0] == "partition" && trailingSlash:
		links := []map[string]string{}
		for id := range f.files {
			links = append(links, map[string]string{"link": fmt.Sprintf("%s/partition/%s/file/%s", f.server.URL, segs[1], id)})
		}
		writeJSON(w, 200, map[string]interface{}{"files": links})
	case req.Method == http.MethodPost && len(segs) == 2 && segs[0] == "partition":
		writeJSON(w, 201, map[string]string{})
	case len(segs) == 4 && segs[0] == "partition" && segs[2] == "file":
		ff, ok := f.files[segs[3]]
		if req.Method != http.MethodGet {
			writeJSON(w, 405, nil)
		} else if !ok {
			writeJSON(w, 404, nil)
		} else {
			writeJSON(w, 200, map[string]interface{}{"metadata": map[string]string{"md5sum": ff.MD5}})
		}
	case len(segs) == 5 && segs[0] == "indexer" && segs[1] == "partition" && segs[3] == "file":
		id := segs[4]
		switch req.Method {
		case http.MethodDelete:
			if _, ok := f.files[id]; !ok {
				writeJSON(w, 404, nil)
				return
			}
			delete(f.files, id)
			writeJSON(w, 200, map[string]string{})
		case http.MethodPost, http.MethodPut:
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			var wsIDs []string
			if raw := req.FormValue("workspace_ids"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &wsIDs); err != nil {
					writeJSON(w, 400, map[string]string{"error": err.Error()})
					return
				}
				for _, ws := range wsIDs {
					if !f.workspaces[ws] {
						writeJSON(w, 404, map[string]string{"error": "unknown workspace " + ws})
						return
					}
				}
			}
			f.files[id] = &fakeFile{MD5: req.URL.Query().Get("md5sum"), Workspaces: wsIDs}
			writeJSON(w, 200, map[string]string{})
		default:
			writeJSON(w, 405, nil)
		}
	case req.Method == http.MethodPost && len(segs) == 3 && segs[0] == "partition" && segs[2] == "workspaces":
		var body struct {
			WorkspaceID string `json:"workspace_id"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		if f.workspaces[body.WorkspaceID] {
			writeJSON(w, 409, nil)
			return
		}
		f.workspaces[body.WorkspaceID] = true
		writeJSON(w, 201, map[string]string{})
	case len(segs) == 4 && segs[0] == "partition" && segs[2] == "workspaces":
		ws := segs[3]
		switch req.Method {
		case http.MethodGet:
			if f.workspaces[ws] {
				writeJSON(w, 200, map[string]string{"workspace_id": ws})
			} else {
				writeJSON(w, 404, nil)
			}
		case http.MethodDelete:
			if !f.workspaces[ws] {
				writeJSON(w, 404, nil)
				return
			}
			delete(f.workspaces, ws)
			for _, ff := range f.files {
				ff.Workspaces = slices.DeleteFunc(ff.Workspaces, func(s string) bool { return s == ws })
			}
			writeJSON(w, 200, map[string]string{})
		default:
			writeJSON(w, 405, nil)
		}
	case req.Method == http.MethodGet && len(segs) == 5 && segs[0] == "partition" && segs[2] == "files" && segs[4] == "workspaces":
		ff, ok := f.files[segs[3]]
		if !ok {
			writeJSON(w, 404, nil)
			return
		}
		writeJSON(w, 200, map[string]interface{}{"workspace_ids": ff.Workspaces})
	case req.Method == http.MethodPost && len(segs) == 5 && segs[0] == "partition" && segs[2] == "workspaces" && segs[4] == "files":
		ws := segs[3]
		if !f.workspaces[ws] {
			writeJSON(w, 404, nil)
			return
		}
		var body struct {
			FileIDs []string `json:"file_ids"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		for _, id := range body.FileIDs {
			if _, ok := f.files[id]; !ok {
				writeJSON(w, 404, map[string]string{"error": "unknown file " + id})
				return
			}
		}
		for _, id := range body.FileIDs {
			ff := f.files[id]
			if !slices.Contains(ff.Workspaces, ws) {
				ff.Workspaces = append(ff.Workspaces, ws)
			}
		}
		writeJSON(w, 200, map[string]string{})
	case req.Method == http.MethodDelete && len(segs) == 6 && segs[0] == "partition" && segs[2] == "workspaces" && segs[4] == "files":
		ws, id := segs[3], segs[5]
		ff, ok := f.files[id]
		if !ok || !f.workspaces[ws] || !slices.Contains(ff.Workspaces, ws) {
			writeJSON(w, 404, nil)
			return
		}
		ff.Workspaces = slices.DeleteFunc(ff.Workspaces, func(s string) bool { return s == ws })
		writeJSON(w, 200, map[string]string{})
	default:
		f.t.Logf("fake openRAG: unhandled %s %s", req.Method, req.URL.Path)
		writeJSON(w, 404, map[string]string{"error": "unhandled " + req.Method + " " + path.Clean(req.URL.Path)})
	}
}
