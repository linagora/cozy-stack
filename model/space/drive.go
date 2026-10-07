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
func ProvisionDrive(inst *instance.Instance, sp Space) (*sharing.Sharing, error) {
	dir, err := vfs.NewDirDocWithPath(sp.Name, consts.RootDirID, "/", nil)
	if err != nil {
		return nil, err
	}
	dir.AddReferencedBy(couchdb.DocReference{Type: consts.Spaces, ID: sp.ID})
	if err := inst.VFS().CreateDir(dir); err != nil {
		return nil, err
	}

	s, err := sharing.CreateDrive(inst, dir.ID(), sp.Name, "")
	if err != nil {
		return nil, err
	}
	s.OrgDrive = inst.IsOrganizationInstance()
	if _, err := s.Create(inst); err != nil {
		return nil, err
	}

	rec := &Record{
		DocID:          sp.ID,
		OrganizationID: sp.OrganizationID,
		Name:           sp.Name,
		DirID:          dir.ID(),
		SharingID:      s.SID,
		LastEventAt:    sp.Timestamp,
		CreatedAt:      time.Now(),
	}
	if err := couchdb.CreateNamedDocWithDB(inst, rec); err != nil {
		return nil, err
	}
	return s, nil
}
