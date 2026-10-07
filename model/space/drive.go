// Package space provisions the resources of a TwakeSpace space on the
// organization instance. It is the only code that knows space ids: the rest of
// the stack only sees the shared drive.
package space

import (
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// Space is a space as announced by twake.space.created.
type Space struct {
	ID             string
	OrganizationID string
	Name           string
	Timestamp      time.Time
}

// Record keeps, on the organization instance, the drive of a space.
type Record struct {
	DocID          string    `json:"_id,omitempty"`
	DocRev         string    `json:"_rev,omitempty"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	DirID          string    `json:"dir_id"`
	SharingID      string    `json:"sharing_id"`
	LastEventAt    time.Time `json:"last_event_at"`
	CreatedAt      time.Time `json:"created_at"`
}

func (r *Record) ID() string        { return r.DocID }
func (r *Record) Rev() string       { return r.DocRev }
func (r *Record) DocType() string   { return consts.Spaces }
func (r *Record) SetID(id string)   { r.DocID = id }
func (r *Record) SetRev(rev string) { r.DocRev = rev }
func (r *Record) Clone() couchdb.Doc {
	cloned := *r
	return &cloned
}

// ProvisionDrive returns the shared drive of the space on the organization
// instance, creating it on the first call.
//
// The record is written only once the sharing exists, so it always names a
// real drive. A crash before that leaves the folder, found by its reference to
// the space on the next delivery.
func ProvisionDrive(inst *instance.Instance, sp Space) (*sharing.Sharing, error) {
	var rec Record
	err := couchdb.GetDoc(inst, consts.Spaces, sp.ID, &rec)
	if err == nil {
		return sharing.FindSharing(inst, rec.SharingID)
	}
	if !couchdb.IsNotFoundError(err) && !couchdb.IsNoDatabaseError(err) {
		return nil, err
	}

	dir, err := spaceDir(inst, sp)
	if err != nil {
		return nil, err
	}
	s, err := dirDrive(inst, dir)
	if err != nil {
		return nil, err
	}
	if s == nil {
		if s, err = createDrive(inst, dir, sp.Name); err != nil {
			return nil, err
		}
	}

	rec = Record{
		DocID:          sp.ID,
		OrganizationID: sp.OrganizationID,
		Name:           sp.Name,
		DirID:          dir.ID(),
		SharingID:      s.SID,
		LastEventAt:    sp.Timestamp,
		CreatedAt:      time.Now(),
	}
	if err := couchdb.CreateNamedDocWithDB(inst, &rec); err != nil {
		return nil, err
	}
	return s, nil
}

func spaceDir(inst *instance.Instance, sp Space) (*vfs.DirDoc, error) {
	ref := couchdb.DocReference{Type: consts.Spaces, ID: sp.ID}
	req := &couchdb.ViewRequest{
		StartKey: []string{ref.Type, ref.ID},
		EndKey:   []string{ref.Type, ref.ID, couchdb.MaxString},
		Limit:    1,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(inst, couchdb.FilesReferencedByView, req, &res)
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return nil, err
	}
	if len(res.Rows) > 0 {
		return inst.VFS().DirByID(res.Rows[0].ID)
	}

	dir, err := vfs.NewDirDocWithPath(sp.Name, consts.RootDirID, "/", nil)
	if err != nil {
		return nil, err
	}
	dir.AddReferencedBy(ref)
	if err := inst.VFS().CreateDir(dir); err != nil {
		return nil, err
	}
	return dir, nil
}

// dirDrive returns the drive sharing of the folder, or nil when it has none.
func dirDrive(inst *instance.Instance, dir *vfs.DirDoc) (*sharing.Sharing, error) {
	for _, ref := range dir.ReferencedBy {
		if ref.Type != consts.Sharings {
			continue
		}
		s, err := sharing.FindSharing(inst, ref.ID)
		if couchdb.IsNotFoundError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if s.Drive && s.Active {
			return s, nil
		}
	}
	return nil, nil
}

func createDrive(inst *instance.Instance, dir *vfs.DirDoc, name string) (*sharing.Sharing, error) {
	s, err := sharing.CreateDrive(inst, dir.ID(), name, "")
	if err != nil {
		return nil, err
	}
	s.OrgDrive = inst.IsOrganizationInstance()
	if _, err := s.Create(inst); err != nil {
		return nil, err
	}
	return s, nil
}
