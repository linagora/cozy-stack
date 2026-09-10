package rag

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
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
	MD5 string
	// Sha is the sha256 of the content openRAG received, what it
	// deduplicates on inside a partition. It is empty for a file declared
	// with AddFile, which never carried a content.
	Sha        string
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
	// pending holds the ids openRAG has accepted but not indexed yet: a POST
	// on them conflicts, while a GET still answers 404.
	pending map[string]bool
	// Fail, when set, is consulted before every request: a non-zero status
	// is returned as-is without touching the state. Typical use, a file the
	// indexer refuses for good:
	//
	//	f.Fail = func(method, path string) int {
	//		if method == http.MethodPost && path == uploadPath {
	//			return http.StatusUnsupportedMediaType
	//		}
	//		return 0
	//	}
	Fail func(method, path string) int
}

func NewFakeOpenRAG(t *testing.T) *FakeOpenRAG {
	t.Helper()
	f := &FakeOpenRAG{t: t, files: map[string]*FakeFile{}, workspaces: map[string]bool{}, pending: map[string]bool{}}
	f.Server, f.Rec = newRAGTestServer(t, f.handle)
	return f
}

func (f *FakeOpenRAG) AddFile(id, md5 string, workspaces ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[id] = &FakeFile{MD5: md5, Workspaces: append([]string{}, workspaces...)}
}

// AddPendingFile declares a file whose indexing task is still running on
// openRAG: a POST on its id conflicts, but a GET on the file answers 404
// until the task completes (or a PUT re-indexes it).
func (f *FakeOpenRAG) AddPendingFile(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending[id] = true
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
	return FakeFile{MD5: ff.MD5, Sha: ff.Sha, Workspaces: append([]string{}, ff.Workspaces...)}, true
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

// contentSha is the sha256 of the uploaded content, what openRAG
// deduplicates on inside a partition.
func contentSha(req *http.Request) (string, error) {
	part, _, err := req.FormFile("file")
	if err != nil {
		return "", err
	}
	defer part.Close()
	h := sha256.New()
	if _, err := io.Copy(h, part); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileWithContent returns the id of another file holding that content, empty
// when the partition has none. The caller holds the lock.
func (f *FakeOpenRAG) fileWithContent(sha, exceptID string) string {
	if sha == "" {
		return ""
	}
	for id, ff := range f.files {
		if id != exceptID && ff.Sha == sha {
			return id
		}
	}
	return ""
}

// validWorkspaceID is what openRAG accepts as a workspace id: it answers
// 422 on anything else, so a folder id with a dot (the root folder id) can
// never be a workspace id.
var validWorkspaceID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handle routes (after url decoding of the path segments):
//
//	GET    /partition/{d}/                         → {"files":[{"link":".../partition/{d}/file/{id}"}]}
//	DELETE /partition/{d}                          → 204, drops every file and workspace
//	POST   /partition/{d}                          → 201
//	GET    /partition/{d}/file/{id}                → {"metadata":{"md5sum":..}} | 404
//	POST   /indexer/partition/{d}/file/{id}        → 201 (multipart, reads workspace_ids, md5sum from query)
//	                                                 | 409 on a known or pending id
//	                                                 | 409 DOCUMENT_CONTENT_EXISTS when another
//	                                                   file of the partition holds that content
//	PUT    /indexer/partition/{d}/file/{id}        → 202 on a known or pending id | 404
//	DELETE /indexer/partition/{d}/file/{id}        → 200 | 404
//	GET    /partition/{d}/workspaces               → {"workspaces":[{"workspace_id":..}]}
//	GET    /partition/{d}/workspaces/{ws}          → 200 | 404
//	POST   /partition/{d}/workspaces               → 201 | 409
//	DELETE /partition/{d}/workspaces/{ws}          → 200 | 404, and deletes the
//	                                                  files left without workspace
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
	case req.Method == http.MethodDelete && len(segs) == 2 && segs[0] == "partition":
		f.files = map[string]*FakeFile{}
		f.workspaces = map[string]bool{}
		f.pending = map[string]bool{}
		w.WriteHeader(204)
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
				delete(f.pending, id)
				writeJSON(w, 404, nil)
				return
			}
			delete(f.files, id)
			delete(f.pending, id)
			writeJSON(w, 200, map[string]string{})
		case http.MethodPost, http.MethodPut:
			_, known := f.files[id]
			if req.Method == http.MethodPost && (known || f.pending[id]) {
				// openRAG refuses to index an id it already holds, even
				// when its indexing task is still running.
				writeJSON(w, 409, map[string]string{
					"detail": fmt.Sprintf("File '%s' already exists in partition %s", id, segs[2]),
				})
				return
			}
			if req.Method == http.MethodPut && !known && !f.pending[id] {
				// A PUT is a re-index: openRAG does not create the file.
				writeJSON(w, 404, map[string]string{
					"detail": fmt.Sprintf("'%s' not found in partition '%s'", id, segs[2]),
				})
				return
			}
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			sha, err := contentSha(req)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			if req.Method == http.MethodPost {
				if other := f.fileWithContent(sha, id); other != "" {
					// openRAG indexes one document per distinct content in a
					// partition: this id will never be indexed.
					writeJSON(w, 409, map[string]interface{}{
						"detail": fmt.Sprintf("[DOCUMENT_CONTENT_EXISTS]: This document already exists in partition '%s'.", segs[2]),
						"extra": map[string]string{
							"existing_file_id": other,
							"request_id":       "fake-" + id,
						},
					})
					return
				}
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
			f.files[id] = &FakeFile{MD5: req.URL.Query().Get("md5sum"), Sha: sha, Workspaces: wsIDs}
			delete(f.pending, id)
			if req.Method == http.MethodPost {
				writeJSON(w, 201, map[string]interface{}{"task_status_url": "/task/" + id})
				return
			}
			writeJSON(w, 202, map[string]interface{}{"task_status_url": "/task/" + id})
		default:
			writeJSON(w, 405, nil)
		}
	case req.Method == http.MethodGet && len(segs) == 3 && segs[0] == "partition" && segs[2] == "workspaces":
		list := []map[string]string{}
		for id := range f.workspaces {
			list = append(list, map[string]string{"workspace_id": id})
		}
		writeJSON(w, 200, map[string]interface{}{"workspaces": list})
	case req.Method == http.MethodPost && len(segs) == 3 && segs[0] == "partition" && segs[2] == "workspaces":
		var body struct {
			WorkspaceID string `json:"workspace_id"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		if !validWorkspaceID.MatchString(body.WorkspaceID) {
			// openRAG validates the id and answers 422 (FastAPI style).
			writeJSON(w, 422, map[string]interface{}{"detail": []map[string]interface{}{{
				"loc":  []string{"body", "workspace_id"},
				"msg":  "workspace_id must be non-empty and contain only alphanumeric characters, hyphens, or underscores",
				"type": "value_error",
			}}})
			return
		}
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
			for id, ff := range f.files {
				if !slices.Contains(ff.Workspaces, ws) {
					continue
				}
				ff.Workspaces = slices.DeleteFunc(ff.Workspaces, func(s string) bool { return s == ws })
				if len(ff.Workspaces) == 0 {
					// Like openRAG: a member left in no workspace is deleted.
					delete(f.files, id)
				}
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
