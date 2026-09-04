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
	"sync"

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

func Index(inst *instance.Instance, logger logger.Logger, msg IndexMessage) error {
	if msg.Doctype != consts.Files {
		return errors.New("Only file can be indexed for the moment")
	}

	mu := config.Lock().LongOperation(inst, msg.lockName())
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	cp, err := loadCheckpoint(inst, msg.Doctype, msg.checkpointID())
	if err != nil {
		return err
	}
	feed, err := callChangesFeed(inst, msg.Doctype, cp.LastSeq)
	if err != nil {
		return err
	}
	if feed.LastSeq == cp.LastSeq {
		return nil
	}

	// Lazily loaded on first use, so batches that never reconcile workspace
	// membership (e.g. deletions only) skip the CouchDB and openRAG queries.
	kb := sync.OnceValue(func() *kbContext { return loadKBContext(inst, logger) })

	var errj error
	for _, change := range feed.Results {
		if err := callRAGIndexer(inst, msg.Doctype, change, kb, logger); err != nil {
			logger.Warnf("Index error: %s", err)
			errj = errors.Join(errj, err)
		}
	}
	_ = saveCheckpoint(inst, msg.Doctype, msg.checkpointID(), checkpoint{LastSeq: feed.LastSeq})

	if feed.Pending > 0 {
		_ = pushJob(inst, msg.Doctype)
	}

	return errj
}

func callRAGIndexer(inst *instance.Instance, doctype string, change couchdb.Change, kb func() *kbContext, logger logger.Logger) error {
	if strings.HasPrefix(change.DocID, "_design/") {
		return nil
	}

	if change.Doc.Get("type") == consts.DirType {
		// Moving a directory rewrites the path of every descendant directory
		// but never touches the file docs, which may have silently entered or
		// left a knowledge-base folder. Each descendant shows up in this same
		// feed, so reconciling the direct children covers the whole subtree.
		dirPath, _ := change.Doc.Get("path").(string)
		if dirPath != "" && !strings.HasPrefix(dirPath, vfs.TrashDirName) {
			reconcileDirChildren(inst, logger, kb(), change.DocID, dirPath)
		}
		return nil
	}

	// An error only means some sources were unreachable, the flags are usable.
	flags, _ := feature.GetFlags(inst)
	if inst.RAGServer().URL == "" {
		return errors.New("no RAG server configured")
	}

	class, _ := change.Doc.Get("class").(string)
	if !isClassAllowed(flags, class) {
		rev, _ := change.Doc.Get("_rev").(string)
		return SetIndexStatus(inst, change.DocID, StatusNotSupported, rev)
	}

	if change.Deleted || change.Doc.Get("trashed") == true {
		return deleteFromRAG(inst, change.DocID)
	}
	return upsertToRAG(inst, doctype, change, kb, logger)
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
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 500 {
		return fmt.Errorf("DELETE status code: %d", res.StatusCode)
	}
	return nil
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
		return "", false, err
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
		return "", false, fmt.Errorf("GET status code: %d", res.StatusCode)
	}
}

func isIndexed(inst *instance.Instance, docID string) bool {
	var doc IndexStatus
	if err := couchdb.GetDoc(inst, consts.ChatRAG, docID, &doc); err != nil {
		return false
	}
	return doc.Indexed
}

func upsertToRAG(inst *instance.Instance, doctype string, change couchdb.Change, kb func() *kbContext, logger logger.Logger) error {
	md5sum := decodeMD5Sum(change.Doc.Get("md5sum"))
	needed, isNewFile, err := needsIndexation(inst, change.DocID, md5sum)
	if err != nil {
		return err
	}

	dirID, _ := change.Doc.Get("dir_id").(string)
	if !needed {
		// The content did not change but the file may have been
		// moved/renamed: keep the knowledge-base workspaces in sync.
		reconcileMembership(inst, logger, kb(), change.DocID, dirID)
		return nil
	}

	name, content, err := resolveContent(inst, change)
	if err != nil {
		return err
	}
	defer content.Close()

	// The streaming goroutine below needs the workspace membership, but
	// kbContext is not safe for concurrent use: resolve it here, on the
	// indexing loop, before the goroutine starts.
	workspaceIDsJSON, attachUnknown := resolveWorkspaces(inst, logger, kb(), change.DocID, dirID)

	datetime := ""
	if metadata, ok := change.Doc.Get("metadata").(map[string]interface{}); ok {
		datetime, _ = metadata["datetime"].(string)
	}
	rev, _ := change.Doc.Get("_rev").(string)
	meta := map[string]string{
		"md5sum":   md5sum,
		"datetime": datetime,
		"doctype":  doctype,
		// Echoed back by the indexer on the callback, which is ordered on it.
		"doc_rev": rev,
	}

	res, err := uploadToRAG(ragUpload{
		Server:      inst.RAGServer(),
		Domain:      inst.Domain,
		FileID:      change.DocID,
		Name:        name,
		DirID:       dirID,
		MD5Sum:      md5sum,
		Meta:        meta,
		Workspaces:  workspaceIDsJSON,
		CallbackURL: inst.PageURL(IndexStatusPath, nil),
		IsNew:       isNewFile,
	}, content)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 500 {
		return fmt.Errorf("Status code: %d", res.StatusCode)
	}

	if (!isNewFile || attachUnknown) && res.StatusCode < 300 {
		// A re-upload may also have moved the file. And for a new file whose
		// membership could not be resolved before the upload, this is the only
		// chance to attach it, now that the transient error may have cleared.
		reconcileMembership(inst, logger, kb(), change.DocID, dirID)
	}
	return nil
}

// resolveWorkspaces returns the knowledge-base workspaces the file belongs to.
// The boolean tells that they could not be resolved, which is not the same as
// belonging to none.
func resolveWorkspaces(inst *instance.Instance, logger logger.Logger, kbc *kbContext, fileID, dirID string) (string, bool) {
	if kbc.empty() {
		return "", false
	}
	desired, ok := kbc.desiredFor(inst, dirID)
	if !ok {
		logger.Warnf("cannot resolve parent path for file %s (dir %s): skipping workspace attach", fileID, dirID)
		return "", true
	}
	if len(desired) == 0 {
		return "", false
	}
	wsJSON, err := json.Marshal(desired)
	if err != nil {
		return "", false
	}
	return string(wsJSON), false
}

// resolveContent returns what to send to the RAG server. A note is sent as the
// markdown it renders to.
func resolveContent(inst *instance.Instance, change couchdb.Change) (string, io.ReadCloser, error) {
	name, _ := change.Doc.Get("name").(string)
	mime, _ := change.Doc.Get("mime").(string)

	if mime == consts.NoteMimeType {
		metadata, _ := change.Doc.Get("metadata").(map[string]interface{})
		schema, _ := metadata["schema"].(map[string]interface{})
		raw, _ := metadata["content"].(map[string]interface{})
		noteDoc := &note.Document{
			DocID:      change.DocID,
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

	dirID, _ := change.Doc.Get("dir_id").(string)
	internalID, _ := change.Doc.Get("internal_vfs_id").(string)
	f, err := inst.VFS().OpenFile(&vfs.FileDoc{
		Type:       consts.FileType,
		DocID:      change.DocID,
		DirID:      dirID,
		DocName:    name,
		InternalID: internalID,
	})
	if err != nil {
		return "", nil, err
	}
	if strings.HasSuffix(name, consts.DocsExtension) {
		// See https://github.com/OpenLLM-France/RAGondin/issues/88
		name = strings.TrimSuffix(name, consts.DocsExtension) + consts.MarkdownExtension
	}
	return name, f, nil
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
// feed.
func pushJob(inst *instance.Instance, doctype string) error {
	msg, err := job.NewMessage(&IndexMessage{
		Doctype: doctype,
	})
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "rag-index",
		Message:    msg,
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
