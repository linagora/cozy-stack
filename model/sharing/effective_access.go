package sharing

import (
	"os"
	"path"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
)

// SharingScope describes a single sharing scope that applies to a target file
// or folder. It is produced by AccessResolver.scopesFor and consumed by
// Resolve to aggregate an EffectiveAccess. ReadOnly reflects the current
// instance's membership for that scope (via Sharing.MemberFor).
type SharingScope struct {
	SharingID  string
	RootID     string
	RootPath   string
	AccessMode string
	ReadOnly   bool
}

// EffectiveAccess is the merged result of all applicable additive sharing
// scopes for a target, from the perspective of the current instance. CanRead
// is true if the instance is a member of at least one applicable scope;
// CanWrite is true if at least one such scope grants write (highest right
// wins).
type EffectiveAccess struct {
	CanRead          bool
	CanWrite         bool
	SourceSharingIDs []string
}

// Can reports whether the effective access satisfies the given verb. GET
// requires CanRead; POST, PUT, PATCH and DELETE require CanWrite.
func (ea *EffectiveAccess) Can(verb permission.Verb) bool {
	if ea == nil {
		return false
	}
	switch verb {
	case permission.GET:
		return ea.CanRead
	case permission.POST, permission.PUT, permission.PATCH, permission.DELETE:
		return ea.CanWrite
	}
	return false
}

// AccessResolver computes the effective access for a target file or folder,
// covering the target itself (if shared) plus all shared ancestors on its
// path, filtered to scopes where the current instance is a member.
type AccessResolver struct {
	inst *instance.Instance
}

// NewAccessResolver builds an AccessResolver bound to an instance.
func NewAccessResolver(inst *instance.Instance) *AccessResolver {
	return &AccessResolver{inst: inst}
}

// Resolve returns the effective access for the given file or folder. The
// target's own share (if it is a shared file) and the shares of every
// ancestor directory are all considered. Scopes where the current instance
// is not a member are ignored. The highest permission wins: CanRead if at
// least one applicable scope, CanWrite if at least one non-read-only scope.
func (r *AccessResolver) Resolve(targetID string) (*EffectiveAccess, error) {
	scopes, err := r.scopesFor(targetID)
	if err != nil {
		return nil, err
	}
	ea := &EffectiveAccess{SourceSharingIDs: make([]string, 0, len(scopes))}
	for _, sc := range scopes {
		ea.SourceSharingIDs = append(ea.SourceSharingIDs, sc.SharingID)
		ea.CanRead = true
		if !sc.ReadOnly {
			ea.CanWrite = true
		}
	}
	return ea, nil
}

// scopesFor loads the target, builds ancestor paths, finds shared roots on
// the path, adds the target's own file share if any, bulk-loads sharings,
// filters to active additive ones where the current instance is a member,
// and returns the resulting scopes.
func (r *AccessResolver) scopesFor(targetID string) ([]SharingScope, error) {
	fs := r.inst.VFS()

	dir, file, err := fs.DirOrFileByID(targetID)
	if err != nil {
		return nil, err
	}
	if dir == nil && file == nil {
		return nil, os.ErrNotExist
	}

	var targetPath string
	if dir != nil {
		targetPath = dir.Fullpath
	} else {
		targetPath, err = file.Path(fs)
		if err != nil {
			return nil, err
		}
	}

	ancestorPaths := r.ancestorPaths(targetPath, dir != nil)

	// XXX: single find via dir-by-path + Go-side referenced_by filter, rather
	// than a dedicated shared-folder-roots-by-path view — one fewer view to
	// maintain while N ancestors is small.
	paths := make([]any, len(ancestorPaths))
	for i, p := range ancestorPaths {
		paths[i] = p
	}
	type sharedRootDoc struct {
		ID           string                 `json:"_id"`
		Path         string                 `json:"path"`
		ReferencedBy []couchdb.DocReference `json:"referenced_by"`
	}
	var roots []sharedRootDoc
	if len(paths) > 0 {
		req := &couchdb.FindRequest{
			UseIndex: "dir-by-path",
			Selector: mango.And(
				mango.In("path", paths),
				mango.Equal("type", consts.DirType),
				mango.Exists(couchdb.SelectorReferencedBy),
			),
			Fields: []string{"_id", "path", "referenced_by"},
			Limit:  len(paths),
		}
		if err := couchdb.FindDocs(r.inst, consts.Files, req, &roots); err != nil {
			return nil, err
		}
	}

	// Collect (sharingID -> {rootID, rootPath}) from ancestor dir roots.
	type rootInfo struct {
		RootID   string
		RootPath string
	}
	rootBySharing := make(map[string]rootInfo)
	for _, root := range roots {
		for _, ref := range root.ReferencedBy {
			if ref.Type == consts.Sharings {
				if _, ok := rootBySharing[ref.ID]; !ok {
					rootBySharing[ref.ID] = rootInfo{RootID: root.ID, RootPath: root.Path}
				}
			}
		}
	}

	// XXX: DirType selector excludes the file from the Mango find; its
	// own scopes are added by hand so file shares stay additive at the target
	// level.
	if file != nil {
		for _, ref := range file.ReferencedBy {
			if ref.Type == consts.Sharings {
				if _, ok := rootBySharing[ref.ID]; !ok {
					rootBySharing[ref.ID] = rootInfo{RootID: file.DocID, RootPath: targetPath}
				}
			}
		}
	}

	if len(rootBySharing) == 0 {
		return nil, nil
	}

	sharingIDs := make([]string, 0, len(rootBySharing))
	for id := range rootBySharing {
		sharingIDs = append(sharingIDs, id)
	}

	sharings, err := r.loadSharings(sharingIDs)
	if err != nil {
		return nil, err
	}

	scopes := make([]SharingScope, 0, len(sharings))
	for _, s := range sharings {
		info, ok := rootBySharing[s.SID]
		if !ok {
			continue
		}
		member := s.MemberFor(r.inst)
		if member == nil {
			continue
		}
		scopes = append(scopes, SharingScope{
			SharingID:  s.SID,
			RootID:     info.RootID,
			RootPath:   info.RootPath,
			AccessMode: s.EffectiveAccessMode(),
			ReadOnly:   member.ReadOnly,
		})
	}
	return scopes, nil
}

// ancestorPaths returns the directory paths to query for shared roots. When
// the target is a directory, its own path is included (a dir can be a shared
// root). When the target is a file, only its parent directories are included.
func (r *AccessResolver) ancestorPaths(targetPath string, targetIsDir bool) []string {
	var paths []string
	current := path.Clean(targetPath)
	if !targetIsDir {
		current = path.Dir(current)
	}
	for {
		if current == "" || current == "." {
			break
		}
		paths = append(paths, current)
		if current == "/" {
			break
		}
		parent := path.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return paths
}

// loadSharings bulk-loads sharings by ID and keeps only the active additive
// ones. limited_access sharings are dropped in v1. Membership filtering is
// done by the caller (scopesFor) so this helper stays reusable.
func (r *AccessResolver) loadSharings(ids []string) ([]*Sharing, error) {
	sharings, err := FindSharings(r.inst, ids)
	if err != nil {
		return nil, err
	}
	kept := make([]*Sharing, 0, len(sharings))
	for _, s := range sharings {
		if s == nil {
			continue
		}
		if !s.Active {
			continue
		}
		if s.EffectiveAccessMode() != AccessModeAdditive {
			continue
		}
		kept = append(kept, s)
	}
	return kept, nil
}

// NearestRestrictiveBoundary returns the closest limited_access boundary
// applying to the target. v1 has no limited_access mode, so it always
// returns nil.
//
// XXX: reserved for limited_access). v1 returns no boundary; upgrade path is
// to walk ancestors and stop at the first limited_access scope.
func (r *AccessResolver) NearestRestrictiveBoundary(targetID string) (*SharingScope, error) {
	return nil, nil
}

// ChildSharedRootsUnder returns shared roots nested under the given folder.
// v1 has no caller for it yet, so it returns nil.
//
// XXX: reserved for recursive move/copy/delete.
func (r *AccessResolver) ChildSharedRootsUnder(folderID string) ([]SharingScope, error) {
	return nil, nil
}
