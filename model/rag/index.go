package rag

import (
	"bytes"
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
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// BatchSize is the maximal number of documents manipulated at once by the
// worker.
const BatchSize = 100

type IndexMessage struct {
	Doctype string `json:"doctype"`
}

func Index(inst *instance.Instance, logger logger.Logger, msg IndexMessage) error {
	if msg.Doctype != consts.Files {
		return errors.New("Only file can be indexed for the moment")
	}

	mu := config.Lock().ReadWrite(inst, "index/"+msg.Doctype)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	lastSeq, err := getLastSeqNumber(inst, msg.Doctype)
	if err != nil {
		return err
	}
	feed, err := callChangesFeed(inst, msg.Doctype, lastSeq)
	if err != nil {
		return err
	}
	if feed.LastSeq == lastSeq {
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
	_ = updateLastSequenceNumber(inst, msg.Doctype, feed.LastSeq)

	if feed.Pending > 0 {
		_ = pushJob(inst, msg.Doctype)
	}

	return errj
}

func callRAGIndexer(inst *instance.Instance, doctype string, change couchdb.Change, kb func() *kbContext, logger logger.Logger) error {
	if strings.HasPrefix(change.DocID, "_design/") {
		return nil
	}
	flags, err := feature.GetFlags(inst)
	class, _ := change.Doc.Get("class").(string)

	if class == consts.ImageClass {
		// Index images only if flag allows it
		allowImage, ok := flags.M["rag.index.image.enabled"].(bool)
		if !ok || !allowImage {
			return nil
		}
	}
	if class == consts.VideoClass {
		// Index videos only if flag allows it
		allowVideo, ok := flags.M["rag.index.video.enabled"].(bool)
		if !ok || !allowVideo {
			return nil
		}
	}
	if class == consts.AudioClass {
		// Index audio only if flag allows it
		allowAudio, ok := flags.M["rag.index.audio.enabled"].(bool)
		if !ok || !allowAudio {
			return nil
		}
	}
	if change.Doc.Get("type") == consts.DirType {
		// A directory move or rename rewrites the path of the directory and
		// of all its descendant directories, but never touches the file docs
		// they contain: those files may have silently entered or left a
		// knowledge-base folder. Every descendant directory shows up in this
		// same changes feed, so reconciling the direct file children of each
		// changed directory covers the whole moved subtree.
		dirPath, _ := change.Doc.Get("path").(string)
		if dirPath != "" && !strings.HasPrefix(dirPath, vfs.TrashDirName) {
			reconcileDirChildren(inst, logger, kb(), change.DocID, dirPath)
		}
		return nil
	}

	ragServer := inst.RAGServer()
	if ragServer.URL == "" {
		return errors.New("no RAG server configured")
	}
	u, err := url.Parse(ragServer.URL)
	if err != nil {
		return err
	}
	if change.Deleted || change.Doc.Get("trashed") == true {
		// Doc deletion
		res, err := CallRAGQuery(inst, http.MethodDelete, nil, fmt.Sprintf("/indexer/partition/%s/file/%s", inst.Domain, url.PathEscape(change.DocID)), echo.MIMEApplicationJSON)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode >= 500 {
			return fmt.Errorf("DELETE status code: %d", res.StatusCode)
		}
	} else {
		md5sum := fmt.Sprintf("%x", change.Doc.Get("md5sum"))
		res, err := CallRAGQuery(inst, http.MethodGet, nil, fmt.Sprintf("/partition/%s/file/%s", inst.Domain, url.PathEscape(change.DocID)), echo.MIMEApplicationJSON)
		if err != nil {
			return err
		}
		defer res.Body.Close()

		// When the content has not changed, there is no need to regenerate
		// an embedding.
		needIndexation := false
		isNewFile := false
		switch res.StatusCode {
		case 200:
			var response map[string]interface{}
			if err = json.NewDecoder(res.Body).Decode(&response); err != nil {
				return err
			}
			metadata, ok := response["metadata"].(map[string]interface{})
			if !ok {
				needIndexation = true
			}
			md5sumFromRAG, ok := metadata["md5sum"].(string)
			if !ok {
				needIndexation = true
			}
			needIndexation = md5sumFromRAG != md5sum

		case 404:
			needIndexation = true
			isNewFile = true
		default:
			return fmt.Errorf("GET status code: %d", res.StatusCode)
		}
		dirID, _ := change.Doc.Get("dir_id").(string)
		if !needIndexation {
			// The content did not change but the file may have been
			// moved/renamed: keep the knowledge-base workspaces in sync.
			reconcileMembership(inst, logger, kb(), change.DocID, dirID)
			return nil
		}

		name, _ := change.Doc.Get("name").(string)
		mime, _ := change.Doc.Get("mime").(string)
		metadataRaw, ok := change.Doc.Get("metadata").(map[string]interface{})
		datetime := ""
		if ok {
			datetime, _ = metadataRaw["datetime"].(string)
		}
		internalID, _ := change.Doc.Get("internal_vfs_id").(string)
		var content io.Reader

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
				return err
			}
			content = bytes.NewReader(md)
			// See https://github.com/OpenLLM-France/RAGondin/issues/88
			name = strings.TrimSuffix(name, consts.NoteExtension) + consts.MarkdownExtension
		} else {
			fs := inst.VFS()
			fileDoc := &vfs.FileDoc{
				Type:       consts.FileType,
				DocID:      change.DocID,
				DirID:      dirID,
				DocName:    name,
				InternalID: internalID,
			}
			f, err := fs.OpenFile(fileDoc)
			if err != nil {
				return err
			}
			defer f.Close()
			content = f
			if strings.HasSuffix(name, consts.DocsExtension) {
				// See https://github.com/OpenLLM-France/RAGondin/issues/88
				name = strings.TrimSuffix(name, consts.DocsExtension) + consts.MarkdownExtension
			}
		}

		u.RawQuery = url.Values{
			"dir_id": []string{dirID},
			"name":   []string{name},
			"md5sum": []string{md5sum},
		}.Encode()
		u.Path = fmt.Sprintf("/indexer/partition/%s/file/%s", inst.Domain, change.DocID)

		// The streaming goroutine below needs the workspace membership, but
		// kbContext is not safe for concurrent use: resolve it here, on the
		// indexing loop, before the goroutine starts.
		var workspaceIDsJSON string
		attachUnknown := false
		if kbc := kb(); !kbc.empty() {
			if desired, ok := kbc.desiredFor(inst, dirID); !ok {
				logger.Warnf("cannot resolve parent path for file %s (dir %s): skipping workspace attach", change.DocID, dirID)
				attachUnknown = true
			} else if len(desired) > 0 {
				if wsJSON, err := json.Marshal(desired); err == nil {
					workspaceIDsJSON = string(wsJSON)
				}
			}
		}

		// Create pipe and writer for file streaming
		pr, pw := io.Pipe()
		writer := multipart.NewWriter(pw)

		go func() {
			part, err := writer.CreateFormFile("file", name)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := io.Copy(part, content); err != nil {
				_ = pw.CloseWithError(err)
				return
			}

			// No need to add filename here, it is already set through the file form
			meta := map[string]string{
				"md5sum":   md5sum,
				"datetime": datetime,
				"doctype":  doctype,
			}
			ragMetadata, err := json.Marshal(meta)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if err := writer.WriteField("metadata", string(ragMetadata)); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if workspaceIDsJSON != "" {
				if err := writer.WriteField("workspace_ids", workspaceIDsJSON); err != nil {
					_ = pw.CloseWithError(err)
					return
				}
			}
			defer pw.Close()
			defer writer.Close()
		}()

		var req *http.Request
		if isNewFile {
			req, err = http.NewRequest(http.MethodPost, u.String(), pr)
		} else {
			req, err = http.NewRequest(http.MethodPut, u.String(), pr)
		}
		if err != nil {
			return err
		}

		req.Header.Add(echo.HeaderAuthorization, "Bearer "+ragServer.APIKey)
		req.Header.Add("Content-Type", writer.FormDataContentType())

		res, err = ragHTTPClient.Do(req)
		if err != nil {
			return err
		}

		var response map[string]interface{}
		if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
			return err
		}
		defer res.Body.Close()

		if res.StatusCode >= 500 {
			return fmt.Errorf("Status code: %d", res.StatusCode)
		}

		if (!isNewFile || attachUnknown) && res.StatusCode < 300 {
			// For an existing file, the content changed but it may also have
			// been moved/renamed at the same time: keep the knowledge-base
			// workspaces in sync after a successful re-upload. For a new file
			// whose membership could not be resolved before the upload
			// (attachUnknown), this is the only chance to attach it: retry
			// now that the transient path-resolution error may have cleared.
			reconcileMembership(inst, logger, kb(), change.DocID, dirID)
		}
	}
	return nil
}

// getLastSeqNumber returns the last sequence number of the previous
// indexation for this doctype.
func getLastSeqNumber(inst *instance.Instance, doctype string) (string, error) {
	result, err := couchdb.GetLocal(inst, doctype, "rag-index")
	if couchdb.IsNotFoundError(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	seq, _ := result["last_seq"].(string)
	return seq, nil
}

// updateLastSequenceNumber updates the last sequence number for this
// indexation if it's superior to the number in CouchDB.
func updateLastSequenceNumber(inst *instance.Instance, doctype, seq string) error {
	result, err := couchdb.GetLocal(inst, doctype, "rag-index")
	if err != nil {
		if !couchdb.IsNotFoundError(err) {
			return err
		}
		result = make(map[string]interface{})
	} else {
		if prev, ok := result["last_seq"].(string); ok {
			if revision.Generation(seq) <= revision.Generation(prev) {
				return nil
			}
		}
	}
	result["last_seq"] = seq
	return couchdb.PutLocal(inst, doctype, "rag-index", result)
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
