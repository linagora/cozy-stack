package search

import (
	"errors"
	"slices"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// Hit is a search result for which the path to show must be computed.
type Hit struct {
	OwnerInstanceID string
	DocID           string
	DirID           string
	// DriveID is the id of the shared drive sharing, empty for the files of
	// the caller.
	DriveID string
}

// Drive is a shared drive the caller is a member of, as listed by the scope
// docs of the search index.
type Drive struct {
	OwnerInstanceID string
	RootDirID       string
	// RootFileID is the file of a single-file drive (RootDirID is empty).
	RootFileID string
}

// VisiblePath is the path to show for a hit, and whether the hit can be shown.
type VisiblePath struct {
	Path string
	OK   bool
}

// VisiblePaths returns the path of the parent folder of each hit, as the
// caller sees it:
//   - for the files of the caller, the full path of the parent folder;
//   - for the files of a shared drive, the path from the root of the drive,
//     prefixed by the name of the root folder, so that nothing above the root
//     of the drive is shown.
//
// A hit is not OK when its parent folder is missing or in the trash, when it
// is no longer in its drive, when the drive is not in drives (the drives the
// caller is a member of, by sharing id), or when its owner is not an instance
// of this stack cluster. The root folder of a drive, and the file of a
// single-file drive, are OK with an empty path.
func VisiblePaths(caller *instance.Instance, hits []Hit, drives map[string]Drive) ([]VisiblePath, error) {
	out := make([]VisiblePath, len(hits))

	// The folders to load, by instance: the parents, and the drive roots
	wanted := make(map[string][]string)
	for _, hit := range hits {
		if !ownedAsExpected(caller, hit, drives) {
			continue
		}
		wanted[hit.OwnerInstanceID] = append(wanted[hit.OwnerInstanceID], hit.DirID)
		if root := drives[hit.DriveID].RootDirID; hit.DriveID != "" && root != "" {
			wanted[hit.OwnerInstanceID] = append(wanted[hit.OwnerInstanceID], root)
		}
	}

	dirs := make(map[string]map[string]*vfs.DirDoc, len(wanted))
	for domain, ids := range wanted {
		var db prefixer.Prefixer = caller
		if domain != caller.Domain {
			// The global CouchDB knows all the instances of the stack cluster,
			// even those served by another stack server: no HTTP call.
			inst, err := instance.Get(domain)
			if errors.Is(err, instance.ErrNotFound) {
				// The indices only hold docs of this stack cluster: a missing
				// owner is a deleted instance, whose hits are dropped.
				continue
			}
			if err != nil {
				return nil, err
			}
			db = inst
		}
		var err error
		if dirs[domain], err = loadDirs(db, ids); err != nil {
			return nil, err
		}
	}

	for i, hit := range hits {
		if !ownedAsExpected(caller, hit, drives) {
			continue
		}
		parent := dirs[hit.OwnerInstanceID][hit.DirID]
		if parent == nil || vfs.IsInTrash(parent.Fullpath) {
			continue
		}
		if hit.DriveID == "" {
			out[i] = VisiblePath{Path: parent.Fullpath, OK: true}
			continue
		}
		d := drives[hit.DriveID]
		if d.RootFileID != "" {
			// The file is the whole drive: nothing above it is shown
			out[i] = VisiblePath{OK: true}
			continue
		}
		root := dirs[hit.OwnerInstanceID][d.RootDirID]
		if root == nil {
			continue
		}
		if hit.DocID == root.DocID {
			// The root folder itself: nothing above it is shown
			out[i] = VisiblePath{OK: true}
			continue
		}
		scope := sharing.SharingScope{RootID: root.DocID, RootPath: root.Fullpath}
		rel, err := scope.PathFromRoot(parent.Fullpath)
		if err != nil {
			continue // moved out of the drive
		}
		out[i] = VisiblePath{Path: "/" + root.DocName + rel, OK: true}
	}
	return out, nil
}

// VisiblePaths returns the visible path of each hit of the page, as the
// function VisiblePaths does, with the drives of the caller found by Search.
func (r *Results) VisiblePaths(caller *instance.Instance) ([]VisiblePath, error) {
	hits := make([]Hit, len(r.Hits))
	for i, h := range r.Hits {
		hits[i] = h.Hit
	}
	return VisiblePaths(caller, hits, r.Drives)
}

// ownedAsExpected returns true when the hit belongs to the caller, or to the
// owner of its drive. The folders of other instances are loaded for the
// drives only, and an unknown drive has no owner.
func ownedAsExpected(caller *instance.Instance, hit Hit, drives map[string]Drive) bool {
	owner := caller.Domain
	if hit.DriveID != "" {
		owner = drives[hit.DriveID].OwnerInstanceID
	}
	return owner != "" && hit.OwnerInstanceID == owner
}

// loadDirs loads the folders with the given ids, with one request (none for
// no ids).
func loadDirs(db prefixer.Prefixer, ids []string) (map[string]*vfs.DirDoc, error) {
	docs, err := vfs.DirOrFileDocsByIDs(db, slices.Compact(slices.Sorted(slices.Values(ids))))
	if err != nil {
		return nil, err
	}
	dirs := make(map[string]*vfs.DirDoc, len(docs))
	for id, doc := range docs {
		if dir, _ := doc.Refine(); dir != nil {
			dirs[id] = dir
		}
	}
	return dirs, nil
}
