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
)

type RecordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

type RequestRecorder struct {
	mu       sync.Mutex
	requests []RecordedRequest
}

func (r *RequestRecorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.mu.Lock()
	r.requests = append(r.requests, RecordedRequest{Method: req.Method, Path: req.URL.Path, Body: body})
	r.mu.Unlock()
}

func (r *RequestRecorder) All() []RecordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func (r *RequestRecorder) Count(method, p string) int {
	n := 0
	for _, rr := range r.All() {
		if rr.Method == method && rr.Path == p {
			n++
		}
	}
	return n
}

func TestingLogger() logger.Logger {
	return logger.WithNamespace("rag-test")
}

func newRAGTestServer(t *testing.T, handler http.HandlerFunc) (config.RAGServer, *RequestRecorder) {
	t.Helper()
	rec := &RequestRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)
		handler(w, req)
	}))
	t.Cleanup(srv.Close)
	return config.RAGServer{URL: srv.URL, APIKey: "test-key"}, rec
}

// FakeFile is the state openRAG keeps for one indexed file.
type FakeFile struct {
	MD5        string
	Workspaces []string
}

// FakeOpenRAG is an in-memory openRAG implementing the routes the stack uses.
type FakeOpenRAG struct {
	t          *testing.T
	Server     config.RAGServer
	Rec        *RequestRecorder
	mu         sync.Mutex
	files      map[string]*FakeFile
	workspaces map[string]bool
	// Fail, when set, is consulted before every request: a non-zero status
	// is returned as-is without touching the state.
	Fail func(method, path string) int
	// OnUpload, when set, is called after a successful POST/PUT with the
	// file id and the doc_rev found in the metadata form field. Integration
	// tests use it to emulate the indexer's status callback.
	OnUpload func(fileID, docRev string)
}

func NewFakeOpenRAG(t *testing.T) *FakeOpenRAG {
	t.Helper()
	f := &FakeOpenRAG{t: t, files: map[string]*FakeFile{}, workspaces: map[string]bool{}}
	f.Server, f.Rec = newRAGTestServer(t, f.handle)
	return f
}

func (f *FakeOpenRAG) AddFile(id, md5 string, workspaces ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[id] = &FakeFile{MD5: md5, Workspaces: append([]string{}, workspaces...)}
}

func (f *FakeOpenRAG) AddWorkspace(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces[id] = true
}

func (f *FakeOpenRAG) File(id string) (FakeFile, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[id]
	if !ok {
		return FakeFile{}, false
	}
	return FakeFile{MD5: ff.MD5, Workspaces: append([]string{}, ff.Workspaces...)}, true
}

func (f *FakeOpenRAG) HasWorkspace(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workspaces[id]
}

func (f *FakeOpenRAG) FileIDs() []string {
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
func (f *FakeOpenRAG) handle(w http.ResponseWriter, req *http.Request) {
	if f.Fail != nil {
		if status := f.Fail(req.Method, req.URL.Path); status != 0 {
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
			links = append(links, map[string]string{"link": fmt.Sprintf("%s/partition/%s/file/%s", f.Server.URL, segs[1], id)})
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
			f.files[id] = &FakeFile{MD5: req.URL.Query().Get("md5sum"), Workspaces: wsIDs}
			if f.OnUpload != nil {
				var meta struct {
					DocRev string `json:"doc_rev"`
				}
				_ = json.Unmarshal([]byte(req.FormValue("metadata")), &meta)
				f.mu.Unlock()
				f.OnUpload(id, meta.DocRev)
				f.mu.Lock()
			}
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
