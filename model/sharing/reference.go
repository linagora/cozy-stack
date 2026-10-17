package sharing

import (
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// Reference names the object of another app that a drive was provisioned
// for, and the last revision of that object applied to the drive.
type Reference struct {
	Source   string `json:"source"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	// Ready is set once the broker confirmed the reply announcing the drive.
	Ready bool `json:"ready,omitempty"`
	// Activity turns on the activity digest of the drive.
	Activity bool `json:"activity,omitempty"`
}

// ReportsActivity is true for a drive whose changes go to the activity
// digest: the app asked for it and knows the drive.
func (s *Sharing) ReportsActivity() bool {
	return s.Reference != nil && s.Reference.Activity && s.Reference.Ready
}

// Provisioned is true for a drive that another app manages through
// drive.org_drive.ensure. Only its commands may delete, move or rename it.
func (s *Sharing) Provisioned() bool {
	return s.Reference != nil
}

// CheckProvisionedRoot returns ErrProvisionedDrive when dir is the root of a
// provisioned drive, removed ones included: trashing the root of a drive
// deletes its sharing, and with it the revision that keeps late commands
// from bringing it back.
func CheckProvisionedRoot(db prefixer.Prefixer, dir *vfs.DirDoc) error {
	if dir == nil {
		return nil
	}
	for _, ref := range dir.ReferencedBy {
		if ref.Type != consts.Sharings {
			continue
		}
		s, err := FindSharing(db, ref.ID)
		if couchdb.IsNotFoundError(err) {
			continue
		}
		if err != nil {
			return err
		}
		if s.Owner && s.Provisioned() {
			return ErrProvisionedDrive
		}
	}
	return nil
}

// FindByReference returns the sharings provisioned for this object, the
// revoked ones included.
func FindByReference(db prefixer.Prefixer, source, id string) ([]*Sharing, error) {
	req := &couchdb.FindRequest{
		UseIndex: "by-reference",
		Selector: mango.And(
			mango.Equal("reference.source", source),
			mango.Equal("reference.id", id),
		),
		Limit: 100,
	}
	var all []*Sharing
	for {
		var page []*Sharing
		res, err := couchdb.FindDocsRaw(db, consts.Sharings, req, &page)
		if couchdb.IsNoDatabaseError(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < req.Limit {
			return all, nil
		}
		req.Bookmark = res.Bookmark
	}
}
