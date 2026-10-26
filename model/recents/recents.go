// Package recents maintains the personal recents rows of the
// io.cozy.files.recents doctype: one row per (user, item), and a row exists
// only because its user edited or opened the item.
package recents

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
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
