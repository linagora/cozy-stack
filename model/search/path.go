package search

import (
	"errors"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// Hit is a search result for which the path to show must be computed.
type Hit struct {
	OwnerInstanceID string
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
// is no longer in its drive, or when the drive is not in drives (the drives
// the caller is a member of, by sharing id). The
// hits of a drive owned outside of this stack cluster are OK, with an empty
// path.
func VisiblePaths(caller *instance.Instance, hits []Hit, drives map[string]Drive) ([]VisiblePath, error) {
	out := make([]VisiblePath, len(hits))

	// The folders to load, by instance: the parents, and the drive roots
	wanted := make(map[string][]string)
	for _, hit := range hits {
		if hit.DriveID == "" {
			if hit.OwnerInstanceID == caller.Domain {
				wanted[caller.Domain] = append(wanted[caller.Domain], hit.DirID)
			}
			continue
		}
		if d, ok := drives[hit.DriveID]; ok && d.OwnerInstanceID == hit.OwnerInstanceID {
			wanted[d.OwnerInstanceID] = append(wanted[d.OwnerInstanceID], hit.DirID, d.RootDirID)
		}
	}

	dirs := make(map[string]map[string]*vfs.DirDoc, len(wanted))
	remote := make(map[string]bool)
	for domain, ids := range wanted {
		var db prefixer.Prefixer = caller
		if domain != caller.Domain {
			// The global CouchDB knows all the instances of the stack cluster,
			// even those served by another stack server: no HTTP call.
			inst, err := instance.Get(domain)
			if errors.Is(err, instance.ErrNotFound) {
				// shortcut: the hits of a drive owned outside of this stack
				// cluster have no path (only our CouchDB is queried); upgrade
				// with the federated search.
				remote[domain] = true
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
		// The folders of other instances are loaded for the drives only: a
		// hit outside of a drive must belong to the caller.
		d, inDrive := drives[hit.DriveID]
		if hit.DriveID == "" && hit.OwnerInstanceID != caller.Domain ||
			hit.DriveID != "" && (!inDrive || d.OwnerInstanceID != hit.OwnerInstanceID) {
			continue
		}
		if remote[hit.OwnerInstanceID] {
			out[i] = VisiblePath{OK: true}
			continue
		}
		parent := dirs[hit.OwnerInstanceID][hit.DirID]
		if parent == nil || isTrashed(parent.Fullpath) {
			continue
		}
		if hit.DriveID == "" {
			out[i] = VisiblePath{Path: parent.Fullpath, OK: true}
			continue
		}
		root := dirs[hit.OwnerInstanceID][d.RootDirID]
		if root == nil {
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

// loadDirs loads the folders with the given ids, with one request.
func loadDirs(db prefixer.Prefixer, ids []string) (map[string]*vfs.DirDoc, error) {
	var docs []*vfs.DirDoc
	req := &couchdb.AllDocsRequest{Keys: ids}
	if err := couchdb.GetAllDocs(db, consts.Files, req, &docs); err != nil {
		return nil, err
	}
	dirs := make(map[string]*vfs.DirDoc, len(docs))
	for _, doc := range docs {
		if doc != nil && doc.Type == consts.DirType {
			dirs[doc.DocID] = doc
		}
	}
	return dirs, nil
}

func isTrashed(fullpath string) bool {
	return fullpath == vfs.TrashDirName || strings.HasPrefix(fullpath, vfs.TrashDirName+"/")
}
