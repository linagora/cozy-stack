// Package recents maintains the personal recents rows of the
// io.cozy.files.recents doctype: one row per (user, item), and a row exists
// only because its user edited or opened the item.
package recents

import (
	"errors"
	"strconv"
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

// Drive is the shared drive of an item that the user does not own. The caller
// computes it on the instance that holds the item, as this package knows nothing
// of the sharings.
type Drive struct {
	// ID is the ID of the sharing of the drive.
	ID string
	// Path is the path of the item from the root of the drive.
	Path string
}

// SchemaVersion is the version of the recents row format.
const SchemaVersion = 1

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
// personal activity of a user on an item, and what is displayed for this item.
// Its _id is the ID of the file or folder. The dates of activity are RFC 3339
// strings, in UTC and to the second, so that their lexicographic order is their
// chronological order.
type FileDirActivity struct {
	DocID  string `json:"_id,omitempty"`
	DocRev string `json:"_rev,omitempty"`

	// DriveID is the ID of the sharing of the shared drive and OwnerDomain the
	// domain of the instance of its owner, for an item of a drive that the
	// user does not own. They are empty otherwise.
	DriveID     string `json:"drive_id,omitempty"`
	OwnerDomain string `json:"owner_domain,omitempty"`

	LastEditedAt   string `json:"last_edited_at,omitempty"`
	LastViewedAt   string `json:"last_viewed_at,omitempty"`
	LastActivityAt string `json:"last_activity_at"`

	Name      string `json:"name"`
	Type      string `json:"type"`
	Mime      string `json:"mime,omitempty"`
	Class     string `json:"class,omitempty"`
	Size      string `json:"size,omitempty"`
	UpdatedAt string `json:"updated_at"`
	DirID     string `json:"dir_id"`
	// Path starts at the root of the drive for an item of a shared drive.
	Path string `json:"path"`

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

// Metadata is what a row tells of its item, as opposed to the dates of the
// personal activity of its user. DriveID and OwnerDomain are empty, and Path is
// the full path, for a row of the owner of the item.
type Metadata struct {
	DriveID     string
	OwnerDomain string
	Name        string
	Type        string
	Mime        string
	Class       string
	Size        string
	UpdatedAt   string
	DirID       string
	Path        string
}

// DirMetadata returns the metadata of a folder for its owner.
func DirMetadata(dir *vfs.DirDoc) Metadata {
	return Metadata{
		Name:      dir.DocName,
		Type:      dir.Type,
		UpdatedAt: dir.UpdatedAt.Format(time.RFC3339Nano),
		DirID:     dir.DirID,
		Path:      dir.Fullpath,
	}
}

// FileMetadata returns the metadata of a file for its owner, given its full
// path, which a file document does not hold.
func FileMetadata(file *vfs.FileDoc, fullpath string) Metadata {
	return Metadata{
		Name:      file.DocName,
		Type:      file.Type,
		Mime:      file.Mime,
		Class:     file.Class,
		Size:      strconv.FormatInt(file.ByteSize, 10),
		UpdatedAt: file.UpdatedAt.Format(time.RFC3339Nano),
		DirID:     file.DirID,
		Path:      fullpath,
	}
}

func (d *FileDirActivity) metadata() Metadata {
	return Metadata{
		DriveID:     d.DriveID,
		OwnerDomain: d.OwnerDomain,
		Name:        d.Name,
		Type:        d.Type,
		Mime:        d.Mime,
		Class:       d.Class,
		Size:        d.Size,
		UpdatedAt:   d.UpdatedAt,
		DirID:       d.DirID,
		Path:        d.Path,
	}
}

func (d *FileDirActivity) setMetadata(m Metadata) {
	d.DriveID = m.DriveID
	d.OwnerDomain = m.OwnerDomain
	d.Name = m.Name
	d.Type = m.Type
	d.Mime = m.Mime
	d.Class = m.Class
	d.Size = m.Size
	d.UpdatedAt = m.UpdatedAt
	d.DirID = m.DirID
	d.Path = m.Path
}

// activityItem is an item on which an activity must be recorded.
type activityItem struct {
	id       string
	metadata Metadata
}

// newActivityItem returns the item of an activity, or nil when the activity
// must not be recorded: an item with a fixed ID, an item of another instance
// outside of a drive of the user, or an actor without the recents enabled.
func newActivityItem(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc, drive *Drive) (*activityItem, error) {
	var id string
	switch {
	case dir != nil:
		id = dir.ID()
	case file != nil:
		id = file.ID()
	default:
		return nil, errors.New("recents: an activity needs a file or a folder")
	}
	own := actor.Domain == holder.Domain
	if IsFixedID(id) || (!own && drive == nil) || !Enabled(actor) {
		return nil, nil
	}

	var meta Metadata
	if dir != nil {
		meta = DirMetadata(dir)
	} else {
		meta = FileMetadata(file, "")
	}

	switch {
	case !own:
		// The full path on the holder must never reach the row: it names the
		// folders above the root of the drive.
		meta.DriveID = drive.ID
		meta.OwnerDomain = holder.Domain
		meta.Path = drive.Path
	case file != nil:
		fullpath, err := file.Path(holder.VFS())
		if err != nil {
			return nil, err
		}
		meta.Path = fullpath
	}
	return &activityItem{id: id, metadata: meta}, nil
}

// RecordActivity upserts the personal recents row of the actor on an item,
// with the metadata of the item. The actor is the person doing the activity, as
// their instance: the row lives in their io.cozy.files.recents database. The
// holder is the instance of the item, that is the instance of the actor for
// their own item. The item is the file or the folder, the other one being nil,
// as it is once written. The drive is required for an item of another instance,
// and ignored for an own item. The date is stamped by the server here: a single
// stack means a single clock.
//
// Nothing is recorded for an item with a fixed ID, for an item of another
// instance without a drive, or when the recents are not enabled for the actor.
func RecordActivity(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc, kind Kind, drive *Drive) error {
	item, err := newActivityItem(actor, holder, dir, file, drive)
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

func recordActivity(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc, kind Kind, drive *Drive, at time.Time) error {
	item, err := newActivityItem(actor, holder, dir, file, drive)
	if err != nil || item == nil {
		return err
	}
	_, err = upsertActivity(actor, item, kind, at)
	return err
}

// activityCacheSize bounds the per-process memory of the stored activities.
const activityCacheSize = 10000

// storedActivity is what this process knows to be stored on a row for a kind
// of activity: a date of that kind at least stamp, with this metadata.
type storedActivity struct {
	stamp    string
	metadata Metadata
}

var (
	activityCache     *expirable.LRU[string, storedActivity]
	initActivityCache sync.Once
)

// storedActivities is the memory of the activities that this process wrote,
// or found already stored, in the last minRewriteInterval. A hit means that
// the row already holds an activity of this kind less than minRewriteInterval
// old, with the same metadata: nothing would be written, so the hot paths skip
// the CouchDB read.
//
// A row deleted or changed outside of RecordActivity cannot be rewritten by
// this process while the entry lives. No legitimate activity can target such
// a row in that window: a row is removed when its item is destroyed, whose ID
// is never reused, or when the instance of the actor is reset by an import,
// which keeps it blocked much longer. The TTL is part of that bound, as a stored
// date can be in the future: it must stay minRewriteInterval.
func storedActivities() *expirable.LRU[string, storedActivity] {
	initActivityCache.Do(func() {
		activityCache = expirable.NewLRU[string, storedActivity](activityCacheSize, nil, minRewriteInterval)
	})
	return activityCache
}

func activityCacheKey(actor *instance.Instance, id string, kind Kind) string {
	return actor.DocID + "|" + id + "|" + string(kind)
}

// recentlyStored reports whether this process knows that the row of the actor
// already holds this activity: same metadata, and a stored date of this kind
// less than minRewriteInterval before stamp.
func recentlyStored(actor *instance.Instance, item *activityItem, kind Kind, stamp string) bool {
	s, ok := storedActivities().Get(activityCacheKey(actor, item.id, kind))
	return ok && s.metadata == item.metadata && !isNewer(stamp, s.stamp)
}

func rememberStored(actor *instance.Instance, doc *FileDirActivity, kind Kind) {
	if stamp := doc.stampOf(kind); stamp != "" {
		storedActivities().Add(activityCacheKey(actor, doc.DocID, kind),
			storedActivity{stamp: stamp, metadata: doc.metadata()})
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
// returns the row as stored, written or left unchanged.
func upsertActivity(actor *instance.Instance, item *activityItem, kind Kind, at time.Time) (*FileDirActivity, error) {
	stamp := formatStamp(at)

	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var doc FileDirActivity
		err = couchdb.GetDoc(actor, consts.FilesRecents, item.id, &doc)
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			doc = FileDirActivity{DocID: item.id, SchemaVersion: SchemaVersion}
			doc.setMetadata(item.metadata)
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
		changed := mergeActivity(&doc, kind, stamp)
		if doc.metadata() != item.metadata {
			doc.setMetadata(item.metadata)
			changed = true
		}
		if !changed {
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
