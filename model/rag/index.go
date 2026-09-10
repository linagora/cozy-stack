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
	"os"
	"strings"
	"time"

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
	// MaxBatchRetries caps the attempts at a batch that keeps failing on
	// retryable errors, so one poisoned file cannot stall a trigger. The
	// worker retries a failed job MaxExecCount times (3, see worker/rag), and
	// every execution consumes one attempt: 15 is about five trigger firings.
	MaxBatchRetries = 15
	workerType      = "rag-index"
	// indexLockName serializes the rag-index jobs of an instance.
	indexLockName = "index/" + consts.Files
	// checkpointDocID is the CouchDB local document holding the checkpoint.
	checkpointDocID = "rag-index"
)

// IndexMessage is the message of rag-index triggers and jobs. Without
// ReconcileDirID the job processes the changes feed; with it, the job walks
// the folder's subtree (initial indexing of a new knowledge base folder).
type IndexMessage struct {
	Doctype        string `json:"doctype"`
	ReconcileDirID string `json:"reconcile_dir_id,omitempty"`
}

// Index is the entry point of the rag-index worker.
func Index(inst *instance.Instance, logger logger.Logger, msg IndexMessage) error {
	if msg.Doctype != consts.Files {
		return errors.New("Only file can be indexed for the moment")
	}
	server := inst.RAGServer()
	if server.URL == "" {
		return errors.New("no RAG server configured")
	}

	mu := config.Lock().LongOperation(inst, indexLockName)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	ctx := &indexContext{inst: inst, logger: logger, server: server}
	// An error only means some sources were unreachable, the flags are usable.
	ctx.flags, _ = feature.GetFlags(inst)
	sc, err := loadScopes(inst, logger)
	if err != nil {
		return err
	}
	ctx.scopes = sc

	// A reconcile job must not spawn reconcile jobs: it is the one doing the
	// walk, and a workspace whose creation keeps failing would loop. A nil
	// push also bounds the reconciliation to the job's own folder.
	var push func(string) error
	if msg.ReconcileDirID == "" {
		push = func(dirID string) error { return pushReconcile(inst, dirID) }
	}
	if err := reconcileWorkspaces(inst, logger, server, sc, msg.ReconcileDirID, push); err != nil {
		if isRetryable(err) {
			// The workspaces of the partition could not be listed, or a
			// removal failed on a transient error: the view is partial, so
			// nothing was detached on a guess and the batch must not run
			// against workspaces that may not exist.
			return err
		}
		// Definitive removal failures: reported, retried next run; the batch
		// still runs.
		logger.Warnf("workspace reconciliation: %s", err)
	}

	if msg.ReconcileDirID != "" {
		return ctx.reconcileFolder(msg.ReconcileDirID)
	}

	cp, err := loadCheckpoint(inst, consts.Files, checkpointDocID)
	if err != nil {
		return err
	}
	feed, err := callChangesFeed(inst, consts.Files, cp.LastSeq)
	if err != nil {
		return err
	}
	if feed.LastSeq == cp.LastSeq {
		return nil
	}
	return ctx.runBatch(cp, feed)
}

// indexContext is the state of one rag-index run.
type indexContext struct {
	inst   *instance.Instance
	logger logger.Logger
	server config.RAGServer
	flags  *feature.Flags
	scopes *scopes
}

// runBatch processes the (at most BatchSize) changes of the feed loaded after
// the checkpoint, then advances the checkpoint unless the batch must be
// retried.
func (ctx *indexContext) runBatch(cp checkpoint, feed *couchdb.ChangesResponse) error {
	var errj error
	var failed []string
	retry := false
	for _, change := range feed.Results {
		if err := ctx.handleChange(change); err != nil {
			if errors.Is(err, errDuplicateContent) {
				// openRAG will never index that content twice: indexFile
				// logged it, and the batch has nothing to report or replay.
				continue
			}
			ctx.logger.Warnf("Index error on %s: %s", change.DocID, err)
			errj = errors.Join(errj, err)
			failed = append(failed, change.DocID)
			if isRetryable(err) {
				retry = true
			}
		}
	}

	if retry && cp.Retries < MaxBatchRetries {
		cp.Retries++
		if err := saveCheckpoint(ctx.inst, consts.Files, checkpointDocID, cp); err != nil {
			errj = errors.Join(errj, err)
		}
		return errj
	}
	if retry {
		ctx.logger.Errorf("Giving up on the batch after %s after %d attempts on %v: %s",
			cp.LastSeq, cp.Retries+1, failed, errj)
	}
	cp.LastSeq = feed.LastSeq
	cp.Retries = 0
	if err := saveCheckpoint(ctx.inst, consts.Files, checkpointDocID, cp); err != nil {
		return errors.Join(errj, err)
	}
	if feed.Pending > 0 {
		_ = pushJob(ctx.inst, IndexMessage{Doctype: consts.Files})
	}
	if retry {
		// Gave up: the job must still report the batch as failed.
		return errj
	}
	// Any remaining failure was non-retryable: it was logged and skipped, and
	// the batch is done. Failing the job would only replay it for nothing.
	return nil
}

func (ctx *indexContext) handleChange(change couchdb.Change) error {
	if strings.HasPrefix(change.DocID, "_design/") {
		return nil
	}
	if change.Doc.Get("type") == consts.DirType {
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
	ctx.scopes.dirs[change.DocID] = dirPath
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
		if err := ctx.handleFile(fileInfoFromDoc(file)); err != nil && !errors.Is(err, errDuplicateContent) {
			errj = errors.Join(errj, err)
		}
	}
}

// handleFile applies the scope rule to one live file: it must be indexed in
// the workspace of every knowledge base folder containing it, and must not
// be on openRAG at all when no folder contains it.
func (ctx *indexContext) handleFile(f fileInfo) error {
	parentPath, err := ctx.scopes.dirs.path(ctx.inst.VFS(), f.DirID)
	if errors.Is(err, os.ErrNotExist) {
		ctx.logger.Warnf("parent directory %s of file %s is gone: skipped", f.DirID, f.ID)
		return nil
	}
	if err != nil {
		// Unknown scope: hold the checkpoint rather than guess.
		return retryable(err)
	}
	desired := ctx.scopes.desiredFor(parentPath)
	if len(desired) > 0 {
		return ctx.indexFile(f, desired)
	}
	if f.fromFeed && strings.HasPrefix(f.Rev, "1-") {
		// The file document was created after the checkpoint and is out of
		// scope: it was never indexed, no need to ask openRAG. A file reached
		// through a directory change does not qualify: moving its parent
		// leaves it at rev 1- even though it may well be indexed.
		return nil
	}
	return deleteFromRAG(ctx.inst, f.ID)
}

// indexFile sends the file to the indexer when openRAG does not hold its
// current content, and keeps its memberships equal to desired (the ids of
// the knowledge base folders containing the file, mapped to workspace ids).
func (ctx *indexContext) indexFile(f fileInfo, desired []string) error {
	if !isClassAllowed(ctx.flags, f.Class) {
		return SetIndexStatus(ctx.inst, f.ID, StatusNotSupported, f.Rev)
	}
	needed, isNew, err := needsIndexation(ctx.inst, f.ID, f.MD5)
	if err != nil {
		return err
	}
	// openRAG knows the folders by their workspace id.
	workspaceIDs := workspaceIDsForDirs(desired)
	if !needed {
		return syncMembership(ctx.server, ctx.inst.Domain, f.ID, workspaceIDs)
	}

	workspaces, err := json.Marshal(workspaceIDs)
	if err != nil {
		return err
	}
	name, content, err := resolveContent(ctx.inst, f)
	if err != nil {
		return contentUnavailable(f.ID, err)
	}
	up := ragUpload{
		Server:      ctx.server,
		Domain:      ctx.inst.Domain,
		FileID:      f.ID,
		Name:        name,
		DirID:       f.DirID,
		MD5Sum:      f.MD5,
		Meta:        uploadMeta(f),
		Workspaces:  string(workspaces),
		CallbackURL: ctx.inst.PageURL(IndexStatusPath, nil),
		IsNew:       isNew,
	}
	status, body, err := sendUpload(up, content)
	if err != nil {
		return retryable(err)
	}
	if status == http.StatusConflict {
		conflict, existingID := classifyConflict(body)
		switch {
		case conflict == conflictContentExists:
			// openRAG holds one document per distinct content in a
			// partition: this file id will never be indexed, and a PUT on
			// it would answer 404. There is nothing to try, now or later —
			// a PUT that is refused the same way is as definitive.
			if existingID == "" {
				existingID = "another file"
			}
			ctx.logger.Infof("file %s (%s) has the same content as %s already indexed: skipped",
				f.ID, f.Name, existingID)
			return errDuplicateContent
		case conflict == conflictIDExists && isNew:
			// openRAG already holds the file id: either the indexing of a
			// previous upload is still running (it answers 404 on the file
			// until the task completes) or a POST was lost on the way back.
			// Both are fixed by a PUT, which re-indexes the content.
			ctx.logger.Infof("file %s is already on the RAG server: uploading it again with a PUT", f.ID)
			// The reader of the first attempt was consumed.
			if _, content, err = resolveContent(ctx.inst, f); err != nil {
				return contentUnavailable(f.ID, err)
			}
			up.IsNew = false
			isNew = false
			if status, _, err = sendUpload(up, content); err != nil {
				return retryable(err)
			}
		}
	}
	if err := statusError("upload", status); err != nil {
		return err
	}
	if !isNew {
		// A PUT re-embeds the content: make sure the memberships survived it.
		return syncMembership(ctx.server, ctx.inst.Domain, f.ID, workspaceIDs)
	}
	return nil
}

// sendUpload sends one upload request and returns its status code with the
// beginning of the body openRAG answered (what tells its two 409 apart). It
// always closes the content.
func sendUpload(up ragUpload, content io.ReadCloser) (int, []byte, error) {
	defer content.Close()
	res, err := uploadToRAG(up, content)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, body, nil
}

// maxErrorBody bounds what is read from the body of an upload response: it is
// read on every response, but only to classify a failure, and openRAG answers
// a short JSON.
const maxErrorBody = 4096

// errDuplicateContent is the file openRAG will never index: another document
// of the partition already holds that content. It is a definitive skip, not a
// failure, and callers count it apart.
var errDuplicateContent = errors.New("content already indexed under another file id")

// uploadConflict is what a 409 of the upload route means.
type uploadConflict int

const (
	// conflictUnknown is a 409 whose body says nothing the stack knows.
	conflictUnknown uploadConflict = iota
	// conflictIDExists: openRAG already holds that file id (indexed, or its
	// indexing task still running). A PUT re-indexes it.
	conflictIDExists
	// conflictContentExists: another id of the partition already holds that
	// content. openRAG deduplicates by content, so this id stays unindexed.
	conflictContentExists
)

// duplicateContentCode is the marker openRAG puts in the detail of the 409 it
// answers on an upload whose content is already indexed under another id.
const duplicateContentCode = "DOCUMENT_CONTENT_EXISTS"

// ragError is the error body of openRAG (FastAPI): a detail, and the extra
// fields of the errors that carry one.
type ragError struct {
	Detail string `json:"detail"`
	Extra  struct {
		ExistingFileID string `json:"existing_file_id"`
	} `json:"extra"`
}

// classifyConflict tells the two 409 of the upload route apart, and returns
// the id of the document already holding the content for the second one:
//
//	{"detail": "File '<id>' already exists in partition <d>"}
//	{"detail": "[DOCUMENT_CONTENT_EXISTS]: This document already exists in
//	 partition '<d>'.", "extra": {"existing_file_id": "<other id>"}}
func classifyConflict(body []byte) (uploadConflict, string) {
	var ragErr ragError
	if err := json.Unmarshal(body, &ragErr); err != nil {
		// Not the expected shape (a FastAPI validation detail is a list, and
		// a proxy may answer something that is not JSON at all): the raw body
		// is all there is to look at.
		ragErr.Detail = string(body)
	}
	switch {
	// The content case comes first: its detail also contains "already
	// exists", so the other order would classify every duplicate as an id
	// conflict and send a PUT that answers 404.
	case strings.Contains(ragErr.Detail, duplicateContentCode):
		return conflictContentExists, ragErr.Extra.ExistingFileID
	case strings.Contains(ragErr.Detail, "already exists"):
		return conflictIDExists, ""
	default:
		return conflictUnknown, ""
	}
}

// reconcileFolder walks a folder's subtree and applies the scope rule to
// every live file: the initial indexing of a new knowledge base folder.
func (ctx *indexContext) reconcileFolder(dirID string) error {
	dir, err := ctx.inst.VFS().DirByID(dirID)
	if errors.Is(err, os.ErrNotExist) {
		ctx.logger.Warnf("reconcile: folder %s does not exist", dirID)
		return nil
	}
	if err != nil {
		return retryable(err)
	}
	// Per-file failures follow the rule of runBatch: a transient one fails
	// the job so that the folder is walked again, a definitive one (the
	// indexer refusing that file) is logged and skipped. Failing the job on
	// the latter would only replay the same walk to the same refusal.
	var firstTransient error
	var indexed, transient, duplicates, skipped int
	err = vfs.WalkAlreadyLocked(ctx.inst.VFS(), dir, func(_ string, _ *vfs.DirDoc, file *vfs.FileDoc, err error) error {
		if err != nil {
			return err
		}
		if file == nil || file.Trashed {
			return nil
		}
		switch err := ctx.handleFile(fileInfoFromDoc(file)); {
		case err == nil:
			indexed++
		case errors.Is(err, errDuplicateContent):
			// A whole Drive holds hundreds of them: indexFile logged the
			// file once, at info, and the walk only counts it here.
			duplicates++
		case isRetryable(err):
			ctx.logger.Warnf("reconcile: file %s failed on a transient error: %s", file.DocID, err)
			transient++
			if firstTransient == nil {
				firstTransient = err
			}
		default:
			ctx.logger.Warnf("reconcile: file %s skipped: %s", file.DocID, err)
			skipped++
		}
		return nil
	})
	if err != nil {
		// The subtree was not walked entirely: nothing else replays it.
		return retryable(err)
	}
	summary := fmt.Sprintf("reconcile: folder %s walked: %d file(s) indexed or up to date, %d duplicate(s) skipped, %d refused",
		dirID, indexed, duplicates, skipped)
	if skipped > 0 {
		ctx.logger.Warn(summary)
	} else {
		ctx.logger.Info(summary)
	}
	if firstTransient != nil {
		return retryable(fmt.Errorf("reconcile: %d file(s) of folder %s failed on a transient error, first: %w",
			transient, dirID, firstTransient))
	}
	return nil
}

// uploadMeta is the metadata form field of an upload, what the RAG server
// stores alongside the file and returns on its search hits.
func uploadMeta(f fileInfo) map[string]string {
	datetime, _ := f.Metadata["datetime"].(string)
	return map[string]string{
		"md5sum":     f.MD5,
		"datetime":   datetime,
		"created_at": f.CreatedAt,
		"doctype":    consts.Files,
		// Echoed back by the indexer on the callback, which is ordered on it.
		"doc_rev": f.Rev,
	}
}

// fileInfo is the subset of a file document the indexer needs, built either
// from a changes feed entry or from a VFS document.
type fileInfo struct {
	ID, Rev, DirID, Name, Mime, Class, MD5, InternalID string
	// CreatedAt is the creation date of the document in RFC 3339, as CouchDB
	// stores it (empty when the document has none).
	CreatedAt string
	Metadata  map[string]interface{}
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
	f.CreatedAt, _ = doc.Get("created_at").(string)
	f.MD5 = decodeMD5Sum(doc.Get("md5sum"))
	f.Metadata, _ = doc.Get("metadata").(map[string]interface{})
	return f
}

func fileInfoFromDoc(doc *vfs.FileDoc) fileInfo {
	return fileInfo{
		ID:         doc.DocID,
		Rev:        doc.DocRev,
		DirID:      doc.DirID,
		Name:       doc.DocName,
		Mime:       doc.Mime,
		Class:      doc.Class,
		InternalID: doc.InternalID,
		CreatedAt:  formatCreatedAt(doc.CreatedAt),
		MD5:        hex.EncodeToString(doc.MD5Sum),
		Metadata:   map[string]interface{}(doc.Metadata),
	}
}

// formatCreatedAt serializes a creation date the way CouchDB carries it, so
// that both fileInfo constructors give the RAG server the same value.
func formatCreatedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
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
// The decision only depends on what the RAG server holds: the index status
// document (filled by the indexer's callback) is informative for clients but
// must not drive re-uploads, as a callback that never arrives would otherwise
// make every run send the file again.
func needsIndexation(inst *instance.Instance, fileID, md5sum string) (needed, isNew bool, err error) {
	indexed, known, err := indexedMD5Sum(inst.RAGServer(), inst.Domain, fileID)
	if err != nil {
		return false, false, err
	}
	if !known {
		return true, true, nil
	}
	return indexed != md5sum, false, nil
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

// contentUnavailable classifies a resolveContent failure. Content missing
// from storage (os.ErrNotExist, opening the file) will not come back: a
// plain, non-retryable error, so the file is skipped like an indexer
// refusal instead of failing the batch or the walk. Any other error (a slow
// filesystem, a transient VFS failure, a note that fails to render) stays
// retryable.
func contentUnavailable(fileID string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("content of file %s is missing: %w", fileID, err)
	}
	return retryable(err)
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

// pushReconcile is a variable so tests can record the pushes.
var pushReconcile = pushReconcileJob

// pushReconcileJob asks for the initial indexing of a knowledge base folder,
// in its own job: the walk of a whole subtree does not belong to the batch.
func pushReconcileJob(inst *instance.Instance, dirID string) error {
	return pushJob(inst, IndexMessage{Doctype: consts.Files, ReconcileDirID: dirID})
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
