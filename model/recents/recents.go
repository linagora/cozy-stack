// Package recents maintains the personal recents rows of the
// io.cozy.files.recents doctype: one row per (user, item), and a row exists
// only because its user edited or opened the item.
package recents

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metrics"
)

// flagEnabled is the rollout feature flag of the server-side personal
// recents: off by default, enabled per instance or context.
const flagEnabled = "drive.recents.enabled"

// Enabled reports whether the server-side personal recents are enabled for
// the instance. It gates the activity capture and the /files/recents
// endpoint.
func Enabled(inst *instance.Instance) bool {
	flags, err := feature.GetFlags(inst)
	if err != nil {
		return false
	}
	enabled, _ := flags.M[flagEnabled].(bool)
	return enabled
}

// Kind is the kind of personal activity recorded on an item.
type Kind string

const (
	// KindEdit is a successful write: file creation, content save, folder
	// creation, or a restore from the trash.
	KindEdit Kind = "edit"
	// KindView is an opening of the item in an editor.
	KindView Kind = "view"
)

// SchemaVersion is the version of the recents row format.
const SchemaVersion = 1

// FileDirActivity is a row of the io.cozy.files.recents doctype: the
// personal activity of a user on an item. Its _id is the resource key. Dates
// are RFC 3339 strings so that their lexicographic order is their
// chronological order.
type FileDirActivity struct {
	DocID  string `json:"_id,omitempty"`
	DocRev string `json:"_rev,omitempty"`

	SourceLocator  string `json:"source_locator"`
	LastEditedAt   string `json:"last_edited_at,omitempty"`
	LastViewedAt   string `json:"last_viewed_at,omitempty"`
	LastActivityAt string `json:"last_activity_at"`
	SchemaVersion  int    `json:"schema_version"`
}

// ID is part of the couchdb.Doc interface.
func (d *FileDirActivity) ID() string { return d.DocID }

// Rev is part of the couchdb.Doc interface.
func (d *FileDirActivity) Rev() string { return d.DocRev }

// DocType is part of the couchdb.Doc interface.
func (d *FileDirActivity) DocType() string { return consts.FilesRecents }

// SetID is part of the couchdb.Doc interface.
func (d *FileDirActivity) SetID(id string) { d.DocID = id }

// SetRev is part of the couchdb.Doc interface.
func (d *FileDirActivity) SetRev(rev string) { d.DocRev = rev }

// Clone is part of the couchdb.Doc interface.
func (d *FileDirActivity) Clone() couchdb.Doc { cloned := *d; return &cloned }

// ResourceKey returns the row identifier of an item: the CouchDB _id of the
// source instance (stable across domain changes) and the file ID. Items of
// different instances never collide, even with the same local file ID.
func ResourceKey(src *instance.Instance, fileID string) string {
	return src.DocID + ":" + fileID
}

// PublicID returns the identifier of a row for the clients: a hash of its
// resource key, so that the internal ID of the source instance, possibly
// another person's instance, never leaves the stack.
func PublicID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:32]
}

// RecordActivity upserts the personal recents row of the actor on an item.
// The actor is the person doing the activity, as their instance: the row lives
// in their io.cozy.files.recents database, whoever owns the item (src). The
// date is stamped by the server here: a single stack means a single clock.
func RecordActivity(actor, src *instance.Instance, fileID string, kind Kind) error {
	if err := recordActivity(actor, src, fileID, kind, time.Now()); err != nil {
		metrics.RecentsWriteFailures.Inc()
		return err
	}
	return nil
}

// maxAttempts bounds the read-merge-write loop when several requests update
// the same row (CouchDB update conflicts).
const maxAttempts = 3

func recordActivity(actor, src *instance.Instance, fileID string, kind Kind, at time.Time) error {
	key := ResourceKey(src, fileID)
	locator := src.Domain + "/" + fileID
	stamp := at.UTC().Format(time.RFC3339)

	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var doc FileDirActivity
		err = couchdb.GetDoc(actor, consts.FilesRecents, key, &doc)
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			doc = FileDirActivity{
				DocID:         key,
				SourceLocator: locator,
				SchemaVersion: SchemaVersion,
			}
			if !mergeActivity(&doc, kind, stamp) {
				return nil
			}
			err = couchdb.CreateNamedDocWithDB(actor, &doc)
			if couchdb.IsConflictError(err) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}

		old := doc
		// The domain of the source instance may have changed since the row
		// was written: the new activity carries the current one.
		moved := doc.SourceLocator != locator
		doc.SourceLocator = locator
		if !mergeActivity(&doc, kind, stamp) && !moved {
			return nil // A replayed or too close activity is a no-op.
		}
		err = couchdb.UpdateDocWithOld(actor, &doc, &old)
		if couchdb.IsConflictError(err) {
			continue
		}
		return err
	}
	return err
}

// minRewriteInterval is the minimal interval between two stored activities
// of the same kind on a row: the hot write paths (typing in a note, a sync
// client uploading files) do not rewrite the row on every request.
const minRewriteInterval = 60 * time.Second

// mergeActivity applies an activity to a row with a max monotone per
// component, then recomputes the sort key: an older activity arriving late
// never downgrades a date. It reports whether the row changed.
func mergeActivity(doc *FileDirActivity, kind Kind, stamp string) bool {
	changed := false
	switch kind {
	case KindEdit:
		if isNewer(stamp, doc.LastEditedAt) {
			doc.LastEditedAt = stamp
			changed = true
		}
	case KindView:
		if isNewer(stamp, doc.LastViewedAt) {
			doc.LastViewedAt = stamp
			changed = true
		}
	}
	doc.LastActivityAt = max(doc.LastEditedAt, doc.LastViewedAt)
	return changed
}

// isNewer reports whether stamp is at least minRewriteInterval after the
// stored stamp of the same kind.
func isNewer(stamp, stored string) bool {
	if stamp <= stored {
		return false
	}
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return true
	}
	prev, err := time.Parse(time.RFC3339, stored)
	if err != nil {
		return true // no stored stamp, or a malformed one
	}
	return at.Sub(prev) >= minRewriteInterval
}
