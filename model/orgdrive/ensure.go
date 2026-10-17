// Package orgdrive applies the commands other apps send to keep shared drives
// on an organization instance in sync with their own objects. It knows the
// apps only by the reference they give.
package orgdrive

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/lock"
)

// ErrMultipleActiveDrives means a reference needs repair before it can be updated.
var ErrMultipleActiveDrives = errors.New("multiple active drives for reference")

// Ensure makes the drive of the reference match a created command, and returns
// the members and groups it could not put on it. It returns a nil drive when
// the command is older than the last one applied. A command with the revision
// already applied is applied again, since a failure after the revision was
// stored would otherwise leave the members unsynced.
func Ensure(inst *instance.Instance, cmd *Command) (*sharing.Sharing, []Skipped, error) {
	mu := referenceLock(inst, cmd.Reference)
	if err := mu.Lock(); err != nil {
		return nil, nil, err
	}
	defer mu.Unlock()

	drive, err := ensureDrive(inst, cmd)
	if err != nil || drive == nil {
		return nil, nil, err
	}
	skipped, err := syncMembers(inst, drive, cmd)
	if err != nil {
		return nil, nil, err
	}
	return drive, skipped, nil
}

func referenceLock(inst *instance.Instance, ref Reference) lock.ErrorLocker {
	return config.Lock().LongOperation(inst, fmt.Sprintf("orgdrive/%q/%q/%q", inst.OrgID, ref.Source, ref.ID))
}

func ensureDrive(inst *instance.Instance, cmd *Command) (*sharing.Sharing, error) {
	all, err := sharing.FindByReference(inst, cmd.Reference.Source, cmd.Reference.ID)
	if err != nil {
		return nil, err
	}
	var last int64
	var drive *sharing.Sharing
	var activeIDs []string
	for _, s := range all {
		if !s.Owner || !s.Drive {
			continue
		}
		if s.Reference.Revision > last {
			last = s.Reference.Revision
		}
		if s.Active {
			activeIDs = append(activeIDs, s.SID)
			drive = s
		}
	}
	if len(activeIDs) > 1 {
		return nil, fmt.Errorf("%w: organization %s reference %q/%q: %s", ErrMultipleActiveDrives,
			inst.OrgID, cmd.Reference.Source, cmd.Reference.ID, strings.Join(activeIDs, ", "))
	}
	if cmd.Revision < last {
		return nil, nil
	}
	if drive != nil {
		ok, err := alive(inst, drive)
		if err != nil {
			return nil, err
		}
		if !ok {
			// A restored folder must not reactivate a drive we have replaced.
			drive.Active = false
			if err := couchdb.UpdateDoc(inst, drive); err != nil {
				return nil, err
			}
			drive = nil
		}
	}
	if drive == nil {
		return create(inst, cmd)
	}

	if drive.Reference.Revision == cmd.Revision && drive.Description == cmd.Name && drive.Reference.Activity == cmd.Activity {
		return drive, nil
	}
	drive.Reference.Revision = cmd.Revision
	drive.Reference.Activity = cmd.Activity
	if drive.Description != cmd.Name {
		err = rename(inst, drive, cmd.Name)
	} else {
		err = couchdb.UpdateDoc(inst, drive)
	}
	if err != nil {
		return nil, err
	}
	return drive, nil
}

// alive is false for a drive that was revoked, or whose folder was trashed or
// deleted. A new drive replaces it.
func alive(inst *instance.Instance, s *sharing.Sharing) (bool, error) {
	if !s.Owner || !s.Drive || !s.Active {
		return false, nil
	}
	rule := s.FirstFilesRule()
	if rule == nil || len(rule.Values) == 0 {
		return false, nil
	}
	dir, err := inst.VFS().DirByID(rule.Values[0])
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !inTrash(dir), nil
}

func inTrash(dir *vfs.DirDoc) bool {
	return dir.Fullpath == vfs.TrashDirName || strings.HasPrefix(dir.Fullpath, vfs.TrashDirName+"/")
}

func create(inst *instance.Instance, cmd *Command) (*sharing.Sharing, error) {
	fs := inst.VFS()
	dir, err := vfs.NewDirDocWithPath(freeName(fs, consts.RootDirID, folderName(cmd.Name)), consts.RootDirID, "/", nil)
	if err != nil {
		return nil, err
	}
	if err := fs.CreateDir(dir); err != nil {
		return nil, err
	}
	s, err := shareDir(inst, dir, cmd)
	if err != nil {
		// Without its sharing, the folder would stay at the root and push the
		// redelivery to "Name (2)".
		_ = fs.DestroyDirAndContent(dir, fs.EnsureErased)
		return nil, err
	}
	return s, nil
}

func shareDir(inst *instance.Instance, dir *vfs.DirDoc, cmd *Command) (*sharing.Sharing, error) {
	s, err := sharing.CreateDrive(inst, dir.ID(), cmd.Name, "")
	if err != nil {
		return nil, err
	}
	s.OrgDrive = inst.IsOrganizationInstance()
	s.Reference = &sharing.Reference{
		Source:   cmd.Reference.Source,
		ID:       cmd.Reference.ID,
		Revision: cmd.Revision,
		Activity: cmd.Activity,
	}
	if _, err := s.Create(inst); err != nil {
		return nil, err
	}
	return s, nil
}

func rename(inst *instance.Instance, s *sharing.Sharing, name string) error {
	fs := inst.VFS()
	dir, err := fs.DirByID(s.FirstFilesRule().Values[0])
	if err != nil {
		return err
	}
	if want := folderName(name); dir.DocName != want {
		newName := freeName(fs, dir.DirID, want)
		if _, err := vfs.ModifyDirMetadata(fs, dir, &vfs.DocPatch{Name: &newName}); err != nil {
			return err
		}
	}
	return s.PatchDescription(inst, name)
}

func folderName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if strings.ContainsRune(vfs.ForbiddenFilenameChars, r) {
			return '-'
		}
		return r
	}, name))
	if name == "" || name == "." || name == ".." {
		return "Drive"
	}
	return name
}

func freeName(fs vfs.VFS, parentID, name string) string {
	if exists, err := fs.GetIndexer().DirChildExists(parentID, name); err == nil && exists {
		return vfs.ConflictName(fs, parentID, name, false)
	}
	return name
}
