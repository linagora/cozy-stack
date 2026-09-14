package rag

import (
	"errors"
	"os"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
	"github.com/cozy/cozy-stack/pkg/logger"
)

const (
	StatusSuccess      = "success"
	StatusError        = "error"
	StatusNotSupported = "notsupported"
)

// IndexStatusPath is the path of the route on which the RAG indexer posts the
// indexation status of a file. It is what inst.PageURL takes to build the
// callback_url given to the indexer.
const IndexStatusPath = "/ai/index/status"

// ragLogger is the instance logger of the rag namespace, for the operations
// run outside a job (a job has its own logger, carrying the job id).
func ragLogger(inst *instance.Instance) logger.Logger {
	return inst.Logger().WithNamespace("rag")
}

// lockIndexStatus guards the read-check-write of a status document.
// couchdb.Upsert overwrites the revision it finds instead of raising a conflict,
// so a caller that decides from what it read must hold this lock until it writes.
func lockIndexStatus(inst *instance.Instance, docID string) (func(), error) {
	mu := config.Lock().ReadWrite(inst, "rag/index-status/"+docID)
	if err := mu.Lock(); err != nil {
		return nil, err
	}
	return mu.Unlock, nil
}

func SetIndexStatus(inst *instance.Instance, docID, newStatus, rev string) error {
	log := ragLogger(inst)

	unlock, err := lockIndexStatus(inst, docID)
	if err != nil {
		return err
	}
	defer unlock()

	// A missing file is a no-op: it may have been deleted before its callback.
	file, err := inst.VFS().FileByID(docID)
	if err != nil {
		if couchdb.IsNotFoundError(err) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	// A trashed file is out of the index: a callback still in flight must not
	// claim it is indexed.
	if file.Trashed {
		return nil
	}

	doc := NewIndexStatus(docID)
	err = couchdb.GetDoc(inst, consts.ChatRAG, docID, doc)
	switch {
	case err == nil:
		if isOutdated(rev, doc.DocRev) {
			log.Debugf("SetIndexStatus: dropping status=%s on %s (outdated revision)", newStatus, docID)
			return nil
		}
	case couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err):
		doc = NewIndexStatus(docID)
	default:
		return err
	}

	doc.DocRev = rev
	applyStatus(doc, newStatus)
	return couchdb.Upsert(inst, doc)
}

// DeleteIndexStatus removes the indexation status of a document. One that never
// had a status is not an error.
func DeleteIndexStatus(inst *instance.Instance, docID string) error {
	unlock, err := lockIndexStatus(inst, docID)
	if err != nil {
		return err
	}
	defer unlock()

	var doc IndexStatus
	err = couchdb.GetDoc(inst, consts.ChatRAG, docID, &doc)
	if err != nil {
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			return nil
		}
		return err
	}
	return couchdb.DeleteDoc(inst, &doc)
}

// isOutdated reports whether a callback is older than the stored status. Two
// callbacks about the same revision describe the same indexation, so the last
// one is kept.
func isOutdated(rev, storedRev string) bool {
	if rev == "" || storedRev == "" {
		return false
	}
	return revision.Generation(rev) < revision.Generation(storedRev)
}

func applyStatus(doc *IndexStatus, newStatus string) {
	now := time.Now()
	doc.Status = newStatus
	switch newStatus {
	case StatusSuccess:
		doc.Indexed = true
		doc.LastSuccessDate = &now
	case StatusError:
		// Indexed is preserved: stays true if the file was previously indexed.
		doc.LastErrorDate = &now
	}
}
