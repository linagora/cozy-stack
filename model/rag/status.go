package rag

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

const (
	RAGStatusSuccess      = "success"
	RAGStatusError        = "error"
	RAGStatusNotSupported = "notsupported"
)

func SetRAGStatus(inst *instance.Instance, fileID, newStatus, md5sum string, timestamp time.Time) error {
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	fs := inst.VFS()
	log := inst.Logger().WithNamespace("rag")

	apply := func(file *vfs.FileDoc) error {
		// Callback about an outdated content version (newer content is now on the
		// file): drop it. It must not touch Status, and it must not claim the file
		// is indexed — a more recent status (e.g. a delete) may say otherwise.
		if isOutdated(file, md5sum) {
			log.Debugf("SetRAGStatus: dropping status=%s on %s (outdated content version)", newStatus, fileID)
			return nil
		}

		// Current content: the most recent callback wins.
		if file.CozyMetadata != nil && file.CozyMetadata.RAG != nil &&
			!isNewerThan(timestamp, file.CozyMetadata.RAG) {
			log.Debugf("SetRAGStatus: dropping status=%s on %s (a newer status is already stored)", newStatus, fileID)
			return nil
		}
		newdoc := ensureRAG(file, inst.Domain)
		applyRAGStatus(newdoc.CozyMetadata.RAG, newStatus, timestamp)
		// Bypass fs.UpdateFileDoc on purpose: we only touch cozyMetadata, not the
		// file tree.
		return couchdb.UpdateDoc(fs, newdoc)
	}

	// A missing file is a no-op: it may have been deleted before its callback.
	fetch := func() (*vfs.FileDoc, bool, error) {
		file, err := fs.FileByID(fileID)
		if err != nil {
			if couchdb.IsNotFoundError(err) || errors.Is(err, os.ErrNotExist) {
				return nil, false, nil
			}
			return nil, false, err
		}
		return file, true, nil
	}

	file, ok, err := fetch()
	if err != nil || !ok {
		return err
	}
	err = apply(file)
	if err == nil || !couchdb.IsConflictError(err) {
		return err
	}

	// Retry once on conflict; the worker's automatic retry covers a rare second
	// conflict, so no loop is needed here.
	file, ok, err = fetch()
	if err != nil || !ok {
		return err
	}
	if err := apply(file); err != nil {
		return err
	}
	log.Debugf("SetRAGStatus: 409 conflict on %s resolved, applied status=%s", fileID, newStatus)
	return nil
}

// isOutdated reports whether the callback targets a content version older than
// the file's current content. md5sum is the content hash in hex, as echoed by
// the indexer. An empty md5sum is treated as the current version.
func isOutdated(file *vfs.FileDoc, md5sum string) bool {
	return md5sum != "" && md5sum != fmt.Sprintf("%x", file.MD5Sum)
}

// isNewerThan reports whether ts is more recent than the last status recorded on
// rag, so an out-of-order callback does not overwrite a newer one.
func isNewerThan(ts time.Time, rag *vfs.RAGMetadata) bool {
	var latest time.Time
	if rag.LastSuccessDate != nil && rag.LastSuccessDate.After(latest) {
		latest = *rag.LastSuccessDate
	}
	if rag.LastErrorDate != nil && rag.LastErrorDate.After(latest) {
		latest = *rag.LastErrorDate
	}
	return ts.After(latest)
}

// ensureRAG clones file and makes sure its cozyMetadata.RAG is set, ready to be
// mutated and persisted.
func ensureRAG(file *vfs.FileDoc, domain string) *vfs.FileDoc {
	newdoc := file.Clone().(*vfs.FileDoc)
	if newdoc.CozyMetadata == nil {
		newdoc.CozyMetadata = vfs.NewCozyMetadata(domain)
	}
	if newdoc.CozyMetadata.RAG == nil {
		newdoc.CozyMetadata.RAG = &vfs.RAGMetadata{}
	}
	return newdoc
}

func applyRAGStatus(rag *vfs.RAGMetadata, newStatus string, timestamp time.Time) {
	rag.Status = newStatus
	switch newStatus {
	case RAGStatusSuccess:
		rag.Indexed = true
		rag.LastSuccessDate = &timestamp
	case RAGStatusError:
		// Indexed is preserved: stays true if the file was previously indexed.
		rag.LastErrorDate = &timestamp
	}
}
