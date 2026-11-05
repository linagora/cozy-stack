// Package recents maintains the personal recents rows of the
// io.cozy.files.recents doctype: one row per (user, item), and a row exists
// only because its user edited or opened the item.
package recents

import (
	"strings"

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
// Its _id is the ID of the file or folder. Dates are RFC 3339 strings so that
// their lexicographic order is their chronological order.
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
