// Package space provisions the resources of a TwakeSpace space on the
// organization instance. It is the only code that knows space ids: the rest of
// the stack only sees the shared drive.
package space

import (
	"errors"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/orgdirectory"
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
	Members        []Member
	Timestamp      time.Time
}

// Member is a user of a space with their role in it.
type Member struct {
	UUID  string
	Email string
	Role  string
}

// The roles of a space member.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

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
// instance, creating it on the first call, and shares it with the members not
// on it yet. Members already on it keep their access.
func ProvisionDrive(inst *instance.Instance, sp Space) (*sharing.Sharing, error) {
	s, err := spaceDrive(inst, sp)
	if err != nil {
		return nil, err
	}
	if err := shareWith(inst, s, sp.Members); err != nil {
		return nil, err
	}
	return s, nil
}

// spaceDrive writes the record only once the sharing exists, so a record always
// names a real drive. A crash before that leaves the folder, found by its
// reference to the space on the next delivery.
func spaceDrive(inst *instance.Instance, sp Space) (*sharing.Sharing, error) {
	var rec Record
	err := couchdb.GetDoc(inst, consts.Spaces, sp.ID, &rec)
	switch {
	case err == nil:
		s, err := activeDrive(inst, rec.SharingID)
		if err != nil {
			return nil, err
		}
		if s != nil {
			if sp.Timestamp.After(rec.LastEventAt) {
				rec.LastEventAt = sp.Timestamp
				err = couchdb.UpdateDoc(inst, &rec)
			}
			return s, err
		}
	case !couchdb.IsNotFoundError(err) && !couchdb.IsNoDatabaseError(err):
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

	rec.OrganizationID = sp.OrganizationID
	rec.Name = sp.Name
	rec.DirID = dir.ID()
	rec.SharingID = s.SID
	if sp.Timestamp.After(rec.LastEventAt) {
		rec.LastEventAt = sp.Timestamp
	}
	if rec.DocRev != "" {
		err = couchdb.UpdateDoc(inst, &rec)
	} else {
		rec.DocID = sp.ID
		rec.CreatedAt = time.Now()
		err = couchdb.CreateNamedDocWithDB(inst, &rec)
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// activeDrive returns the drive sharing with this id, or nil when it has been
// revoked or deleted.
func activeDrive(inst *instance.Instance, sharingID string) (*sharing.Sharing, error) {
	s, err := sharing.FindSharing(inst, sharingID)
	if couchdb.IsNotFoundError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !s.Drive || !s.Active {
		return nil, nil
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

	fs := inst.VFS()
	name := folderName(sp)
	if exists, err := fs.GetIndexer().DirChildExists(consts.RootDirID, name); err != nil {
		return nil, err
	} else if exists {
		name = vfs.ConflictName(fs, consts.RootDirID, name, false)
	}
	dir, err := vfs.NewDirDocWithPath(name, consts.RootDirID, "/", nil)
	if err != nil {
		return nil, err
	}
	dir.AddReferencedBy(ref)
	if err := fs.CreateDir(dir); err != nil {
		return nil, err
	}
	return dir, nil
}

func folderName(sp Space) string {
	name := strings.TrimSpace(strings.Map(func(r rune) rune {
		if strings.ContainsRune(vfs.ForbiddenFilenameChars, r) {
			return '-'
		}
		return r
	}, sp.Name))
	if name == "" || name == "." || name == ".." {
		return sp.ID
	}
	return name
}

// dirDrive returns the drive sharing of the folder, or nil when it has none.
func dirDrive(inst *instance.Instance, dir *vfs.DirDoc) (*sharing.Sharing, error) {
	for _, ref := range dir.ReferencedBy {
		if ref.Type != consts.Sharings {
			continue
		}
		if s, err := activeDrive(inst, ref.ID); s != nil || err != nil {
			return s, err
		}
	}
	return nil, nil
}

// shareWith skips a member with no organization-directory contact on the
// instance: the next sync of the space adds them.
func shareWith(inst *instance.Instance, s *sharing.Sharing, members []Member) error {
	log := inst.Logger().WithNamespace("space")
	var readWrite, readOnly []string
	for _, m := range members {
		if isMember(s, m.Email) {
			continue
		}
		c, err := orgdirectory.FindManagedContactByEmail(inst, m.Email)
		if errors.Is(err, contact.ErrNotFound) {
			log.Infof("No contact for space member %s, skipped", m.UUID)
			continue
		}
		if err != nil {
			return err
		}
		switch m.Role {
		case RoleAdmin, RoleEditor:
			readWrite = append(readWrite, c.ID())
		case RoleViewer:
			readOnly = append(readOnly, c.ID())
		default:
			log.Warnf("Unknown role %q for space member %s, skipped", m.Role, m.UUID)
		}
	}
	for _, ids := range []struct {
		contacts []string
		readOnly bool
	}{{readWrite, false}, {readOnly, true}} {
		if len(ids.contacts) == 0 {
			continue
		}
		err := s.AddGroupsAndContacts(inst, nil, ids.contacts, ids.readOnly)
		if errors.Is(err, sharing.ErrInvitationNotSent) {
			log.Warnf("Some invitations to the drive %s were not sent", s.SID)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func isMember(s *sharing.Sharing, email string) bool {
	for _, m := range s.Members[1:] {
		if strings.EqualFold(m.Email, email) {
			return true
		}
	}
	return false
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
