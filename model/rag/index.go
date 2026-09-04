package rag

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

const (
	// BatchSize is the maximal number of documents manipulated at once by the
	// worker.
	BatchSize = 100
	// maxBatchRetries caps the attempts at a batch that keeps failing on
	// retryable errors, so one poisoned file cannot stall a trigger.
	maxBatchRetries = 5
	// ActionRemove asks the worker to detach a folder's files from its
	// workspace, delete the unclaimed ones, and drop the workspace.
	ActionRemove = "remove"
	workerType   = "rag-index"
)

// IndexMessage is the message of rag-index triggers and jobs. Without DirID
// the whole instance is indexed; with DirID only the files under that folder
// (recursively) are, and they are attached to the openRAG workspace named
// after the folder id.
type IndexMessage struct {
	Doctype string `json:"doctype"`
	DirID   string `json:"dir_id,omitempty"`
	Action  string `json:"action,omitempty"`
}

func (m IndexMessage) lockName() string {
	if m.DirID == "" {
		return "index/" + m.Doctype
	}
	return "index/" + m.Doctype + "/" + m.DirID
}

func (m IndexMessage) checkpointID() string {
	if m.DirID == "" {
		return "rag-index"
	}
	return "rag-index-" + m.DirID
}

// Index is the entry point of the rag-index worker.
func Index(inst *instance.Instance, logger logger.Logger, msg IndexMessage) error {
	if msg.Doctype != consts.Files {
		return errors.New("Only file can be indexed for the moment")
	}

	mu := config.Lock().LongOperation(inst, msg.lockName())
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	switch msg.Action {
	case "":
	case ActionRemove:
		if msg.DirID == "" {
			return errors.New("the remove action requires a dir_id")
		}
		return removeFolder(inst, logger, msg.DirID)
	default:
		return fmt.Errorf("unknown rag-index action %q", msg.Action)
	}

	ctx, err := newIndexContext(inst, logger, msg.DirID)
	if err != nil || ctx == nil {
		return err
	}
	return ctx.runBatch(msg)
}

// indexContext is the state of one rag-index run.
type indexContext struct {
	inst   *instance.Instance
	logger logger.Logger
	server config.RAGServer
	flags  *feature.Flags
	scope  *scope // nil for the global job
}

// newIndexContext returns (nil, nil) when a scoped job's folder is gone.
func newIndexContext(inst *instance.Instance, logger logger.Logger, dirID string) (*indexContext, error) {
	server := inst.RAGServer()
	if server.URL == "" {
		return nil, errors.New("no RAG server configured")
	}
	// An error only means some sources were unreachable, the flags are usable.
	flags, _ := feature.GetFlags(inst)
	ctx := &indexContext{inst: inst, logger: logger, server: server, flags: flags}
	if dirID != "" {
		sc, err := newScope(inst, server, logger, dirID)
		if err != nil {
			return nil, err
		}
		if sc == nil {
			return nil, nil
		}
		ctx.scope = sc
	}
	return ctx, nil
}

// runBatch processes up to BatchSize changes after the checkpoint.
func (ctx *indexContext) runBatch(msg IndexMessage) error {
	cp, err := loadCheckpoint(ctx.inst, msg.Doctype, msg.checkpointID())
	if err != nil {
		return err
	}
	feed, err := callChangesFeed(ctx.inst, msg.Doctype, cp.LastSeq)
	if err != nil {
		return err
	}
	if feed.LastSeq == cp.LastSeq {
		return nil
	}

	var errj error
	retry := false
	for _, change := range feed.Results {
		if err := ctx.handleChange(change); err != nil {
			ctx.logger.Warnf("Index error on %s: %s", change.DocID, err)
			errj = errors.Join(errj, err)
			if isRetryable(err) {
				retry = true
			}
		}
	}

	if retry && cp.Retries < maxBatchRetries {
		cp.Retries++
		if err := saveCheckpoint(ctx.inst, msg.Doctype, msg.checkpointID(), cp); err != nil {
			errj = errors.Join(errj, err)
		}
		return errj
	}
	if retry {
		ctx.logger.Errorf("Giving up on the batch after %s after %d attempts: %s", cp.LastSeq, cp.Retries+1, errj)
	}
	cp.LastSeq = feed.LastSeq
	cp.Retries = 0
	if err := saveCheckpoint(ctx.inst, msg.Doctype, msg.checkpointID(), cp); err != nil {
		errj = errors.Join(errj, err)
	}
	if feed.Pending > 0 {
		_ = pushJob(ctx.inst, msg)
	}
	return errj
}

func (ctx *indexContext) handleChange(change couchdb.Change) error {
	if strings.HasPrefix(change.DocID, "_design/") {
		return nil
	}
	if change.Doc.Get("type") == consts.DirType {
		if ctx.scope == nil {
			return nil
		}
		return ctx.handleDirChange(change)
	}
	if change.Deleted || change.Doc.Get("trashed") == true {
		return deleteFromRAG(ctx.inst, change.DocID)
	}
	return ctx.handleFile(fileInfoFromChange(change))
}

// handleDirChange reacts to a directory document change (typically a move
// or a rename): the file documents of the subtree do not change, so the
// direct file children of every changed directory are re-evaluated against
// the scope. Descendant directories show up in the same feed.
func (ctx *indexContext) handleDirChange(change couchdb.Change) error {
	if strings.HasPrefix(change.Doc.Rev(), "1-") {
		return nil // a new directory is empty
	}
	dirPath, _ := change.Doc.Get("path").(string)
	if dirPath == "" {
		return nil
	}
	ctx.scope.cachePath(change.DocID, dirPath)
	iter := ctx.inst.VFS().DirIterator(&vfs.DirDoc{DocID: change.DocID, Fullpath: dirPath}, nil)
	var errj error
	for {
		_, file, err := iter.Next()
		if errors.Is(err, vfs.ErrIteratorDone) {
			return errj
		}
		if err != nil {
			// The remaining children were not evaluated against the scope,
			// and nothing else will bring them back: hold the checkpoint so
			// the whole directory is re-listed on the next run.
			return errors.Join(errj, retryable(err))
		}
		if file == nil || file.Trashed {
			continue
		}
		if err := ctx.handleFile(fileInfoFromDoc(file)); err != nil {
			errj = errors.Join(errj, err)
		}
	}
}

// handleFile applies the scope rule to one live file.
func (ctx *indexContext) handleFile(f fileInfo) error {
	if ctx.scope == nil {
		return ctx.indexFile(f, "")
	}
	parentPath, ok := ctx.scope.parentPath(ctx.inst, f.DirID)
	if !ok {
		ctx.logger.Warnf("cannot resolve parent path for file %s (dir %s): skipped", f.ID, f.DirID)
		return nil
	}
	if ctx.scope.contains(parentPath) {
		return ctx.indexFile(f, ctx.scope.dirID)
	}
	if f.fromFeed && strings.HasPrefix(f.Rev, "1-") {
		// The file document was created after the checkpoint and is out of
		// scope: it was never indexed, no need to ask openRAG. A file reached
		// through a directory change does not qualify: moving its parent
		// leaves it at rev 1- even though it may well be indexed.
		return nil
	}
	return detachFile(ctx.inst, f.ID, ctx.scope.dirID, ctx.scope.globalExists, ctx.logger)
}

// indexFile sends the file to the indexer when openRAG does not hold its
// current content, and keeps its membership in workspaceID (when not empty).
func (ctx *indexContext) indexFile(f fileInfo, workspaceID string) error {
	if !isClassAllowed(ctx.flags, f.Class) {
		return SetIndexStatus(ctx.inst, f.ID, StatusNotSupported, f.Rev)
	}
	needed, isNew, err := needsIndexation(ctx.inst, f.ID, f.MD5)
	if err != nil {
		return err
	}
	if !needed {
		if workspaceID != "" {
			return ensureMembership(ctx.server, ctx.inst.Domain, workspaceID, f.ID)
		}
		return nil
	}

	name, content, err := resolveContent(ctx.inst, f)
	if err != nil {
		return err
	}
	defer content.Close()

	workspaces := ""
	if workspaceID != "" {
		ids, err := json.Marshal([]string{workspaceID})
		if err != nil {
			return err
		}
		workspaces = string(ids)
	}
	meta := map[string]string{
		"md5sum":   f.MD5,
		"datetime": f.Datetime,
		"doctype":  consts.Files,
		// Echoed back by the indexer on the callback, which is ordered on it.
		"doc_rev": f.Rev,
	}
	res, err := uploadToRAG(ragUpload{
		Server:      ctx.server,
		Domain:      ctx.inst.Domain,
		FileID:      f.ID,
		Name:        name,
		DirID:       f.DirID,
		MD5Sum:      f.MD5,
		Meta:        meta,
		Workspaces:  workspaces,
		CallbackURL: ctx.inst.PageURL(IndexStatusPath, nil),
		IsNew:       isNew,
	}, content)
	if err != nil {
		return retryable(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if err := statusError("upload", res.StatusCode); err != nil {
		return err
	}
	if !isNew && workspaceID != "" {
		// A PUT re-embeds the content: make sure the membership survived it.
		return ensureMembership(ctx.server, ctx.inst.Domain, workspaceID, f.ID)
	}
	return nil
}

// fileInfo is the subset of a file document the indexer needs, built either
// from a changes feed entry or from a VFS document.
type fileInfo struct {
	ID, Rev, DirID, Name, Mime, Class, MD5, InternalID, Datetime string
	Metadata                                                     map[string]interface{}
	// fromFeed tells that the file document itself is one of the changes of
	// the batch, as opposed to being reached by iterating a changed
	// directory. Only then does its revision say something about how recent
	// the file is.
	fromFeed bool
}

func fileInfoFromChange(change couchdb.Change) fileInfo {
	doc := change.Doc
	f := fileInfo{ID: change.DocID, Rev: doc.Rev(), fromFeed: true}
	f.DirID, _ = doc.Get("dir_id").(string)
	f.Name, _ = doc.Get("name").(string)
	f.Mime, _ = doc.Get("mime").(string)
	f.Class, _ = doc.Get("class").(string)
	f.InternalID, _ = doc.Get("internal_vfs_id").(string)
	f.MD5 = decodeMD5Sum(doc.Get("md5sum"))
	f.Metadata, _ = doc.Get("metadata").(map[string]interface{})
	f.Datetime, _ = f.Metadata["datetime"].(string)
	return f
}

func fileInfoFromDoc(doc *vfs.FileDoc) fileInfo {
	f := fileInfo{
		ID:         doc.DocID,
		Rev:        doc.DocRev,
		DirID:      doc.DirID,
		Name:       doc.DocName,
		Mime:       doc.Mime,
		Class:      doc.Class,
		InternalID: doc.InternalID,
		MD5:        hex.EncodeToString(doc.MD5Sum),
		Metadata:   map[string]interface{}(doc.Metadata),
	}
	f.Datetime, _ = f.Metadata["datetime"].(string)
	return f
}

func isClassAllowed(flags *feature.Flags, class string) bool {
	switch class {
	case consts.ImageClass:
		allowed, _ := flags.M["rag.index.image.enabled"].(bool)
		return allowed
	case consts.VideoClass:
		allowed, _ := flags.M["rag.index.video.enabled"].(bool)
		return allowed
	case consts.AudioClass:
		allowed, _ := flags.M["rag.index.audio.enabled"].(bool)
		return allowed
	}
	return true
}

func deleteFromRAG(inst *instance.Instance, fileID string) error {
	if err := deleteFromRAGHTTP(inst.RAGServer(), inst.Domain, fileID); err != nil {
		return err
	}
	return DeleteIndexStatus(inst, fileID)
}

func deleteFromRAGHTTP(server config.RAGServer, domain, fileID string) error {
	path := fmt.Sprintf("/indexer/partition/%s/file/%s", domain, url.PathEscape(fileID))
	res, err := callRAG(server, http.MethodDelete, nil, path, echo.MIMEApplicationJSON)
	if err != nil {
		return retryable(err)
	}
	res.Body.Close()
	return statusError("DELETE file", res.StatusCode, http.StatusNotFound)
}

// needsIndexation reports whether the file must be sent again. isNew tells
// whether the RAG server knows it at all, which decides between a POST and a PUT.
func needsIndexation(inst *instance.Instance, fileID, md5sum string) (needed, isNew bool, err error) {
	indexed, known, err := indexedMD5Sum(inst.RAGServer(), inst.Domain, fileID)
	if err != nil {
		return false, false, err
	}
	if !known {
		return true, true, nil
	}
	if indexed != md5sum {
		return true, false, nil
	}
	// The RAG server holds this content, but a callback may never have come
	// back to say so.
	return !isIndexed(inst, fileID), false, nil
}

// indexedMD5Sum returns the md5sum the RAG server holds for the file. known is
// false when it does not know the file at all.
func indexedMD5Sum(server config.RAGServer, domain, fileID string) (md5sum string, known bool, err error) {
	path := fmt.Sprintf("/partition/%s/file/%s", domain, url.PathEscape(fileID))
	res, err := callRAG(server, http.MethodGet, nil, path, echo.MIMEApplicationJSON)
	if err != nil {
		return "", false, retryable(err)
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
		var response map[string]interface{}
		if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
			return "", false, err
		}
		metadata, _ := response["metadata"].(map[string]interface{})
		md5sum, _ = metadata["md5sum"].(string)
		return md5sum, true, nil
	case http.StatusNotFound:
		return "", false, nil
	default:
		return "", false, statusError("GET file", res.StatusCode)
	}
}

func isIndexed(inst *instance.Instance, docID string) bool {
	var doc IndexStatus
	if err := couchdb.GetDoc(inst, consts.ChatRAG, docID, &doc); err != nil {
		return false
	}
	return doc.Indexed
}

// resolveContent returns what to send to the RAG server. A note is sent as the
// markdown it renders to.
func resolveContent(inst *instance.Instance, f fileInfo) (string, io.ReadCloser, error) {
	name := f.Name

	if f.Mime == consts.NoteMimeType {
		schema, _ := f.Metadata["schema"].(map[string]interface{})
		raw, _ := f.Metadata["content"].(map[string]interface{})
		noteDoc := &note.Document{
			DocID:      f.ID,
			SchemaSpec: schema,
			RawContent: raw,
		}
		md, err := noteDoc.Markdown(nil)
		if err != nil {
			return "", nil, err
		}
		// See https://github.com/OpenLLM-France/RAGondin/issues/88
		name = strings.TrimSuffix(name, consts.NoteExtension) + consts.MarkdownExtension
		return name, io.NopCloser(bytes.NewReader(md)), nil
	}

	file, err := inst.VFS().OpenFile(&vfs.FileDoc{
		Type:       consts.FileType,
		DocID:      f.ID,
		DirID:      f.DirID,
		DocName:    name,
		InternalID: f.InternalID,
	})
	if err != nil {
		return "", nil, err
	}
	if strings.HasSuffix(name, consts.DocsExtension) {
		// See https://github.com/OpenLLM-France/RAGondin/issues/88
		name = strings.TrimSuffix(name, consts.DocsExtension) + consts.MarkdownExtension
	}
	return name, file, nil
}

type ragUpload struct {
	Server      config.RAGServer
	Domain      string
	FileID      string
	Name        string
	DirID       string
	MD5Sum      string
	Meta        map[string]string
	Workspaces  string
	CallbackURL string
	IsNew       bool
}

func uploadToRAG(up ragUpload, content io.Reader) (*http.Response, error) {
	u, err := url.Parse(up.Server.URL)
	if err != nil {
		return nil, err
	}
	u.Path = fmt.Sprintf("/indexer/partition/%s/file/%s", up.Domain, up.FileID)
	u.RawQuery = url.Values{
		"dir_id": []string{up.DirID},
		"name":   []string{up.Name},
		"md5sum": []string{up.MD5Sum},
	}.Encode()

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("file", up.Name)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, content); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		// No need to add filename here, it is already set through the file form
		ragMetadata, err := json.Marshal(up.Meta)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		fields := map[string]string{
			"metadata":     string(ragMetadata),
			"callback_url": up.CallbackURL,
		}
		if up.Workspaces != "" {
			fields["workspace_ids"] = up.Workspaces
		}
		for field, value := range fields {
			if err := writer.WriteField(field, value); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	method := http.MethodPut
	if up.IsNew {
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, u.String(), pr)
	if err != nil {
		return nil, err
	}
	req.Header.Add(echo.HeaderAuthorization, "Bearer "+up.Server.APIKey)
	req.Header.Add("Content-Type", writer.FormDataContentType())
	return ragHTTPClient.Do(req)
}

const md5Length = 16

// decodeMD5Sum turns the md5sum carried by the changes feed into the
// hexadecimal digest the RAG server is given. CouchDB serializes the bytes of
// the digest in base64.
func decodeMD5Sum(v interface{}) string {
	s, _ := v.(string)
	if s == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != md5Length {
		return ""
	}
	return hex.EncodeToString(raw)
}

// callChangesFeed fetches the last changes from the changes feed
// http://docs.couchdb.org/en/stable/api/database/changes.html
func callChangesFeed(inst *instance.Instance, doctype, since string) (*couchdb.ChangesResponse, error) {
	return couchdb.GetChanges(inst, &couchdb.ChangesRequest{
		DocType:     doctype,
		IncludeDocs: true,
		Since:       since,
		Limit:       BatchSize,
	})
}

// pushJob adds a new job to continue on the pending documents in the changes
// feed, with the same message.
func pushJob(inst *instance.Instance, msg IndexMessage) error {
	m, err := job.NewMessage(&msg)
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: workerType,
		Message:    m,
	})
	return err
}

func CleanInstance(inst *instance.Instance) error {
	if inst.RAGServer().URL == "" {
		return nil
	}
	res, err := CallRAGQuery(inst, http.MethodDelete, nil, fmt.Sprintf("/instances/%s", inst.Domain), echo.MIMEApplicationJSON)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 500 {
		return fmt.Errorf("DELETE status code: %d", res.StatusCode)
	}
	return nil
}
