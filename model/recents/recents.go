// Package recents maintains the personal recents rows of the
// io.cozy.files.recents doctype: one row per (user, item), and a row exists
// only because its user edited or opened the item.
package recents

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
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

// SchemaVersion is the version of the recents row format. The version 1 also
// stored the size, the modification date, the parent, the path and the drive
// of the item, which are now read from its io.cozy.files document.
const SchemaVersion = 2

// fixedIDPrefix starts the IDs that every instance shares: the root, the
// trash and the system folders.
const fixedIDPrefix = consts.Files + "."

// IsFixedID reports whether an ID is one of the fixed IDs shared by every
// instance. Unlike the other file and folder IDs, it does not identify an item
// across the instances of the stack.
func IsFixedID(id string) bool {
	return strings.HasPrefix(id, fixedIDPrefix)
}

// FileDirActivity is a row of the io.cozy.files.recents doctype: the
// personal activity of a user on an item. Its _id is the ID of the file or
// folder. The dates of activity are RFC 3339 strings, in UTC and to the second,
// so that their lexicographic order is their chronological order.
type FileDirActivity struct {
	DocID  string `json:"_id,omitempty"`
	DocRev string `json:"_rev,omitempty"`

	// OwnerDomain is the domain of the instance that holds the item, for an
	// item of a shared drive that the user does not own. It is empty otherwise.
	OwnerDomain string `json:"owner_domain,omitempty"`

	LastEditedAt   string `json:"last_edited_at,omitempty"`
	LastViewedAt   string `json:"last_viewed_at,omitempty"`
	LastActivityAt string `json:"last_activity_at"`

	// Name, Type, Mime and Class are those of the item at the last written
	// activity of the user, and are not kept up to date otherwise: they are
	// what is left to display when the item cannot be read any more.
	Name  string `json:"name"`
	Type  string `json:"type"`
	Mime  string `json:"mime,omitempty"`
	Class string `json:"class,omitempty"`

	SchemaVersion int `json:"schema_version"`
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

// activityItem is an item on which an activity must be recorded, as the user
// sees it at the time of the activity.
type activityItem struct {
	id          string
	ownerDomain string
	name        string
	docType     string
	mime        string
	class       string
}

// describe writes on the row what the item is at the time of an activity.
func (d *FileDirActivity) describe(item *activityItem) {
	d.OwnerDomain = item.ownerDomain
	d.Name = item.name
	d.Type = item.docType
	d.Mime = item.mime
	d.Class = item.class
}

// newActivityItem returns the item of an activity, or nil when the activity
// must not be recorded: an item with a fixed ID, an item of another instance
// outside of a drive of the user, or an actor without the recents enabled.
func newActivityItem(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc, inDrive bool) (*activityItem, error) {
	var item activityItem
	switch {
	case dir != nil:
		item = activityItem{id: dir.ID(), name: dir.DocName, docType: consts.DirType}
	case file != nil:
		item = activityItem{
			id:      file.ID(),
			name:    file.DocName,
			docType: consts.FileType,
			mime:    file.Mime,
			class:   file.Class,
		}
	default:
		return nil, errors.New("recents: an activity needs a file or a folder")
	}
	own := actor.Domain == holder.Domain
	if IsFixedID(item.id) || (!own && !inDrive) || !Enabled(actor) {
		return nil, nil
	}
	if !own {
		item.ownerDomain = holder.Domain
	}
	return &item, nil
}

// RecordActivity upserts the personal recents row of the actor on an item,
// with the name, the type, the MIME type and the class of the item. The actor
// is the person doing the activity, as their instance: the row lives in their
// io.cozy.files.recents database. The holder is the instance of the item, that
// is the instance of the actor for their own item. The item is the file or the
// folder, the other one being nil, as it is once written. The date is stamped
// by the server here: a single stack means a single clock.
//
// inDrive tells that a shared drive of the actor gives them the item. The
// caller computes it on the holder, as this package knows nothing of the
// sharings. It is required for an item of another instance, and ignored for an
// own item.
//
// Nothing is recorded for an item with a fixed ID, for an item of another
// instance outside of a drive, or when the recents are not enabled for the
// actor.
func RecordActivity(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc, kind Kind, inDrive bool) error {
	item, err := newActivityItem(actor, holder, dir, file, inDrive)
	if err != nil || item == nil {
		return err
	}
	at := time.Now()
	if recentlyStored(actor, item, kind, formatStamp(at)) {
		return nil
	}
	stored, err := upsertActivity(actor, item, kind, at)
	if err != nil {
		return err
	}
	rememberStored(actor, stored, kind)
	return nil
}

func recordActivity(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc, kind Kind, inDrive bool, at time.Time) error {
	item, err := newActivityItem(actor, holder, dir, file, inDrive)
	if err != nil || item == nil {
		return err
	}
	_, err = upsertActivity(actor, item, kind, at)
	return err
}

// activityCacheSize bounds the per-process memory of the stored activities.
const activityCacheSize = 10000

// activityCache maps a row and a kind of activity to what this process knows
// to be stored: a date of that kind at least this stamp.
var (
	activityCache     *expirable.LRU[string, string]
	initActivityCache sync.Once
)

// storedActivities is the memory of the activities that this process wrote,
// or found already stored, in the last minRewriteInterval. A hit means that
// the row already holds an activity of this kind less than minRewriteInterval
// old: nothing would be written, so the hot paths skip the CouchDB read.
//
// A row deleted or changed outside of RecordActivity cannot be rewritten by
// this process while the entry lives. No legitimate activity can target such
// a row in that window: a row is removed when its item is destroyed, whose ID
// is never reused, or when the instance of the actor is reset by an import,
// which keeps it blocked much longer. The TTL is part of that bound, as a stored
// date can be in the future: it must stay minRewriteInterval.
func storedActivities() *expirable.LRU[string, string] {
	initActivityCache.Do(func() {
		activityCache = expirable.NewLRU[string, string](activityCacheSize, nil, minRewriteInterval)
	})
	return activityCache
}

func activityCacheKey(actor *instance.Instance, id string, kind Kind) string {
	return actor.DocID + "|" + id + "|" + string(kind)
}

// recentlyStored reports whether this process knows that the row of the actor
// already holds this activity: a stored date of this kind less than
// minRewriteInterval before stamp.
func recentlyStored(actor *instance.Instance, item *activityItem, kind Kind, stamp string) bool {
	stored, ok := storedActivities().Get(activityCacheKey(actor, item.id, kind))
	return ok && !isNewer(stamp, stored)
}

func rememberStored(actor *instance.Instance, doc *FileDirActivity, kind Kind) {
	if stamp := doc.stampOf(kind); stamp != "" {
		storedActivities().Add(activityCacheKey(actor, doc.DocID, kind), stamp)
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

func formatStamp(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// upsertActivity applies an activity to the row of the actor on an item, and
// returns the row as stored, written or left unchanged. Only a date makes a
// write, and each write describes the item as it is: within the minimal
// interval, an item that has just been renamed keeps its previous name.
func upsertActivity(actor *instance.Instance, item *activityItem, kind Kind, at time.Time) (*FileDirActivity, error) {
	stamp := formatStamp(at)

	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var doc FileDirActivity
		err = couchdb.GetDoc(actor, consts.FilesRecents, item.id, &doc)
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			doc = FileDirActivity{DocID: item.id, SchemaVersion: SchemaVersion}
			doc.describe(item)
			mergeActivity(&doc, kind, stamp)
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
		if !mergeActivity(&doc, kind, stamp) {
			return &old, nil // A replayed or too close activity is a no-op.
		}
		doc.describe(item)
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
// of the same kind on a row: two activities closer than that are one, and
// the date of the first one is kept.
const minRewriteInterval = 60 * time.Second

// mergeActivity applies an activity to a row with a max monotone per
// component, then recomputes the sort key: an older activity arriving late
// never downgrades a date. It reports whether a date changed.
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
