package sharing

import (
	"errors"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// FilePathScope holds the roots visible to a sharing token and its drive, if any.
// Empty roots suppress paths. With nil roots, the drive or instance supplies them.
type FilePathScope struct {
	Roots []vfs.LinkRoot
	Drive *Sharing
}

// Relative returns the path visible in this scope.
func (scope FilePathScope) Relative(p string) string {
	if scope.Roots != nil {
		return vfs.RelativeToLink(scope.Roots, p)
	}
	if scope.Drive != nil {
		return scope.Drive.DriveRelativePath(p)
	}
	return p
}

// Absolute returns the VFS path corresponding to a path visible in this scope.
func (scope FilePathScope) Absolute(p string) string {
	if scope.Roots != nil {
		return vfs.AbsoluteFromLink(scope.Roots, p)
	}
	if scope.Drive != nil {
		return scope.Drive.DriveAbsolutePath(p)
	}
	return p
}

// singleFileRoot returns the one distinct file ID covered by the permission.
func singleFileRoot(perm *permission.Permission) string {
	var id string
	for _, rule := range perm.Permissions {
		if !permission.MatchType(rule, consts.Files) {
			continue
		}
		if !isIDSelector(rule.Selector) || len(rule.Values) == 0 {
			return ""
		}
		for _, value := range rule.Values {
			if value == "" || (id != "" && value != id) {
				return ""
			}
			id = value
		}
	}
	return id
}

// LimitDrivePaths applies the visiting token's path boundary to a drive request.
func (s *Sharing) LimitDrivePaths(perm *permission.Permission) {
	switch perm.Type {
	case permission.TypeShareByLink, permission.TypeSharePreview:
		s.startPathsAt(singleFileRoot(perm))
	case permission.TypeShareInteract:
		if root, err := s.DriveRootID(); err != nil || root != singleFileRoot(perm) {
			s.startPathsAt("")
		}
	}
}

// ResolveFilePaths finds the roots for HTTP responses and realtime events.
func ResolveFilePaths(inst *instance.Instance, perm *permission.Permission) (FilePathScope, error) {
	var scope FilePathScope
	switch perm.Type {
	case permission.TypeShareByLink, permission.TypeSharePreview:
		roots, err := vfs.LinkRoots(inst.VFS(), perm.Permissions)
		scope.Roots = roots
		return scope, err
	case permission.TypeShareInteract:
		scope.Roots = []vfs.LinkRoot{}
		id := singleFileRoot(perm)
		sharingID, ok := strings.CutPrefix(perm.SourceID, consts.Sharings+"/")
		if !ok || sharingID == "" {
			return scope, nil
		}
		s, err := FindSharing(inst, sharingID)
		if couchdb.IsNotFoundError(err) {
			return scope, nil
		}
		if err != nil {
			return scope, err
		}
		if s.Drive {
			scope.Drive = s
			s.driveRootLoaded = true
			if root, err := s.DriveRootID(); err != nil || root != id {
				return scope, nil
			}
		}
		if id == "" {
			return scope, nil
		}
		p, err := vfs.PathByID(inst.VFS(), id)
		if errors.Is(err, os.ErrNotExist) {
			return scope, nil
		}
		if err == nil {
			scope.Roots = []vfs.LinkRoot{{ID: id, Path: p}}
			s.driveRootPath = p
		}
		return scope, err
	default:
		return scope, nil
	}
}
