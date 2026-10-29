// Package recents maintains the personal recents rows of the
// io.cozy.files.recents doctype: one row per (user, item), and a row exists
// only because its user edited or opened the item.
package recents

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metrics"
	"github.com/hashicorp/golang-lru/v2/expirable"
)

// flagEnabled is the rollout feature flag of the server-side personal
// recents: off by default, enabled per instance or context.
const flagEnabled = "drive.recents.enabled"

// Enabled reports whether the server-side personal recents are enabled for
// the instance. It gates the activity capture and the /files/recents
// endpoint.
//
// The flags of the instance win over every other source in
// feature.GetFlags and are read without any I/O. Otherwise, GetFlags reads
// the other sources, as every caller of the stack does. GetFlags only logs a
// source it cannot read and never returns an error: when the flag comes from
// that source, Enabled reports false for this call, and the activity of this
// call is lost.
func Enabled(inst *instance.Instance) bool {
	if v, ok := inst.FeatureFlags[flagEnabled]; ok {
		enabled, _ := v.(bool)
		return enabled
	}
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
//
// A moved source records nothing: the instance that holds the item now has
// another DocID, so a row keyed by the moved one would point to an instance
// that is going away.
func RecordActivity(actor, src *instance.Instance, fileID string, kind Kind) error {
	if src.Moved {
		return nil
	}
	at := time.Now()
	key, locator := rowKey(src, fileID)
	if recentlyStored(actor, key, locator, kind, formatStamp(at)) {
		return nil
	}
	stored, err := upsertActivity(actor, src, fileID, kind, at)
	if err != nil {
		metrics.RecentsActivityFailures.Inc()
		return err
	}
	rememberStored(actor, stored, kind)
	return nil
}

// activityCacheSize bounds the per-process memory of the stored activities.
const activityCacheSize = 10000

// storedActivity is what this process knows to be stored on a row for a kind
// of activity: a date of that kind at least stamp, with this source locator.
type storedActivity struct {
	stamp   string
	locator string
}

var (
	activityCache     *expirable.LRU[string, storedActivity]
	initActivityCache sync.Once
)

// storedActivities is the memory of the activities that this process wrote,
// or found already stored, in the last minRewriteInterval. A hit means that
// the row already holds an activity of this kind less than
// minRewriteInterval old, with the same source locator: mergeActivity would
// write nothing, so the hot write paths (typing in a note, a sync client
// uploading files) skip the CouchDB read.
//
// A row deleted or changed outside of RecordActivity cannot be rewritten by
// this process while the entry lives. No legitimate activity can target such
// a row in that window: a row is removed when its document or its source
// instance is destroyed, whose IDs are never reused, or when the instance of
// the actor is reset by an import, which keeps it blocked much longer. The
// TTL is part of that bound, as a stored date can be in the future: it must
// stay minRewriteInterval.
func storedActivities() *expirable.LRU[string, storedActivity] {
	initActivityCache.Do(func() {
		activityCache = expirable.NewLRU[string, storedActivity](activityCacheSize, nil, minRewriteInterval)
	})
	return activityCache
}

func activityCacheKey(actor *instance.Instance, key string, kind Kind) string {
	return actor.DocID + "|" + key + "|" + string(kind)
}

// recentlyStored reports whether this process knows that the row of the
// actor already holds this activity: same source locator, and a stored date
// of this kind less than minRewriteInterval before stamp.
func recentlyStored(actor *instance.Instance, key, locator string, kind Kind, stamp string) bool {
	s, ok := storedActivities().Get(activityCacheKey(actor, key, kind))
	return ok && s.locator == locator && !isNewer(stamp, s.stamp)
}

func rememberStored(actor *instance.Instance, doc *FileDirActivity, kind Kind) {
	if doc == nil {
		return
	}
	if stamp := doc.stampOf(kind); stamp != "" {
		storedActivities().Add(activityCacheKey(actor, doc.DocID, kind),
			storedActivity{stamp: stamp, locator: doc.SourceLocator})
	}
}

// ResetActivityCacheForTest forgets the stored activities known by this
// process, for the tests that change the rows by hand.
func ResetActivityCacheForTest() {
	storedActivities().Purge()
}

// maxAttempts bounds the read-merge-write loop when several requests update
// the same row (CouchDB update conflicts).
const maxAttempts = 3

func recordActivity(actor, src *instance.Instance, fileID string, kind Kind, at time.Time) error {
	_, err := upsertActivity(actor, src, fileID, kind, at)
	return err
}

// rowKey returns the _id and the source locator of the row of an item.
func rowKey(src *instance.Instance, fileID string) (key, locator string) {
	return ResourceKey(src, fileID), SourceLocator(src, fileID)
}

// SourceLocator returns the source locator of the row of an item: the domain
// of the instance that holds it and its file ID.
func SourceLocator(src *instance.Instance, fileID string) string {
	return src.Domain + "/" + fileID
}

func formatStamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// upsertActivity applies an activity to the row of the actor on an item, and
// returns the row as stored, written or left unchanged.
func upsertActivity(actor, src *instance.Instance, fileID string, kind Kind, at time.Time) (*FileDirActivity, error) {
	key, locator := rowKey(src, fileID)
	stamp := formatStamp(at)

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
				return nil, nil
			}
			err = couchdb.CreateNamedDocWithDB(actor, &doc)
			// The row, or the database, has been created meanwhile by a
			// concurrent activity: the row is read again.
			if couchdb.IsConflictError(err) || couchdb.IsFileExists(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			return &doc, nil
		}
		if err != nil {
			return nil, err
		}

		old := doc
		// The domain of the source instance may have changed since the row
		// was written: the new activity carries the current one.
		moved := doc.SourceLocator != locator
		doc.SourceLocator = locator
		if !mergeActivity(&doc, kind, stamp) && !moved {
			return &old, nil // A replayed or too close activity is a no-op.
		}
		err = couchdb.UpdateDocWithOld(actor, &doc, &old)
		if couchdb.IsConflictError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &doc, nil
	}
	return nil, err
}

// stampOf returns the stored date of a kind of activity.
func (d *FileDirActivity) stampOf(kind Kind) string {
	switch kind {
	case KindEdit:
		return d.LastEditedAt
	case KindView:
		return d.LastViewedAt
	}
	return ""
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
