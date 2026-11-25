package search

import (
	"context"
	"encoding/json"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/opensearch"
)

// sharingsCheckpointDocID is the CouchDB local document holding the
// checkpoint of the io.cozy.sharings changes feed.
const sharingsCheckpointDocID = "search-index-sharings"

// ownedDrive is a shared drive of the instance, with what is needed to know
// if a folder change affects its scope.
type ownedDrive struct {
	sharing  *sharing.Sharing
	rootID   string          // empty when the sharing has no root
	rootPath string          // empty when the root is missing
	keys     map[string]bool // the folder keys of its current scope doc
}

// syncScopes rebuilds the scope docs of the drives of the instance affected by
// the changes of a batch: folders created, moved or deleted in their tree, and
// drive sharings that changed. Each drive is rebuilt at most once.
func syncScopes(ctx context.Context, client *opensearch.Client, inst *instance.Instance, files, sharings []couchdb.Change) error {
	var folders []couchdb.Change
	for _, change := range files {
		if change.Deleted || change.Doc.M["type"] == consts.DirType {
			folders = append(folders, change)
		}
	}
	if len(folders) == 0 && len(sharings) == 0 {
		return nil
	}

	// Only the folder changes need the drives
	var drives []ownedDrive
	if len(folders) > 0 {
		var err error
		if drives, err = loadOwnedDrives(ctx, client, inst); err != nil {
			return err
		}
	}
	rebuild, deleted := affectedDrives(inst.Domain, folders, sharings, drives)
	for _, id := range deleted {
		if err := DeleteScope(ctx, client, inst.Domain, id); err != nil {
			return err
		}
	}
	for _, s := range rebuild {
		doc, keep, err := NewScopeDocument(inst, s)
		if err != nil {
			return err
		}
		if keep {
			err = UpsertScope(ctx, client, doc)
		} else {
			err = DeleteScope(ctx, client, inst.Domain, s.SID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// loadOwnedDrives returns the active drives owned by the instance, with their
// current scope docs and root folders, needed to check the folder changes.
func loadOwnedDrives(ctx context.Context, client *opensearch.Client, inst *instance.Instance) ([]ownedDrive, error) {
	all, err := sharing.ListDrives(inst)
	if err != nil {
		return nil, err
	}
	var drives []ownedDrive
	var ids, rootIDs []string
	for _, s := range all {
		if !s.Owner {
			continue
		}
		rootID, _ := s.DriveRootID()
		drives = append(drives, ownedDrive{sharing: s, rootID: rootID, keys: map[string]bool{}})
		ids = append(ids, s.SID)
		if rootID != "" {
			rootIDs = append(rootIDs, rootID)
		}
	}
	if len(drives) == 0 {
		return nil, nil
	}

	scopes, err := ownedScopes(ctx, client, inst.Domain, ids)
	if err != nil {
		return nil, err
	}
	roots, err := loadDirs(inst, rootIDs)
	if err != nil {
		return nil, err
	}
	for i := range drives {
		if scopes[i] != nil {
			for _, key := range scopes[i].SubtreeDirKeys {
				drives[i].keys[key] = true
			}
		}
		if root := roots[drives[i].rootID]; root != nil {
			drives[i].rootPath = root.Fullpath
		}
	}
	return drives, nil
}

// affectedDrives returns the drives whose scope must be rebuilt, and the ids
// of the deleted sharings whose scope must be removed.
func affectedDrives(domain string, folders, sharings []couchdb.Change, drives []ownedDrive) ([]*sharing.Sharing, []string) {
	var rebuild []*sharing.Sharing
	seen := make(map[string]bool)
	add := func(s *sharing.Sharing) {
		if !seen[s.SID] {
			seen[s.SID] = true
			rebuild = append(rebuild, s)
		}
	}

	for _, change := range folders {
		key := Key(domain, change.DocID)
		folderPath, _ := change.Doc.M["path"].(string)
		for _, d := range drives {
			// Only a folder entering or leaving the tree changes the keys: a
			// rename or a tag inside it does not rebuild the drive. The folders
			// under a skipped folder are left out, as in subdirIDs.
			inTree := !change.Deleted && d.rootPath != "" && !vfs.IsInTrash(folderPath) &&
				(folderPath == d.rootPath ||
					strings.HasPrefix(folderPath, d.rootPath+"/") && !inSkippedTree(path.Dir(folderPath)))
			if inTree != d.keys[key] {
				add(d.sharing)
			}
		}
	}

	var deleted []string
	for _, change := range sharings {
		if strings.HasPrefix(change.DocID, "_design/") {
			continue
		}
		if change.Deleted {
			deleted = append(deleted, change.DocID)
			continue
		}
		s, err := sharingFromChange(change)
		if err != nil {
			logger.WithDomain(domain).WithNamespace("search").
				Warnf("the sharing %s is not read, its scope is not updated: %s", change.DocID, err)
			continue
		}
		if !s.Drive || !s.Owner {
			continue // the recipients have no scope doc
		}
		add(s)
	}
	return rebuild, deleted
}

func sharingFromChange(change couchdb.Change) (*sharing.Sharing, error) {
	data, err := json.Marshal(change.Doc.M)
	if err != nil {
		return nil, err
	}
	var s sharing.Sharing
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
