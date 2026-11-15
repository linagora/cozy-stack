package search

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/opensearch"
)

// sharingsCheckpointDocID is the CouchDB local document holding the
// checkpoint of the io.cozy.sharings changes feed.
const sharingsCheckpointDocID = "search-index-sharings"

// ownedDrive is a shared drive of the instance, with what is needed to know
// if a folder change affects its scope.
type ownedDrive struct {
	sharing  *sharing.Sharing
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

	drives, err := loadOwnedDrives(ctx, client, inst, len(folders) > 0)
	if err != nil {
		return err
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

// loadOwnedDrives returns the active drives owned by the instance. With
// withScopes, it also loads their current scope docs and root folders, needed
// to check the folder changes.
func loadOwnedDrives(ctx context.Context, client *opensearch.Client, inst *instance.Instance, withScopes bool) ([]ownedDrive, error) {
	all, err := sharing.ListDrives(inst)
	if err != nil {
		return nil, err
	}
	var drives []ownedDrive
	for _, s := range all {
		if s.Owner {
			drives = append(drives, ownedDrive{sharing: s, keys: map[string]bool{}})
		}
	}
	if !withScopes || len(drives) == 0 {
		return drives, nil
	}

	ids := make([]string, len(drives))
	var rootIDs []string
	for i, d := range drives {
		ids[i] = d.sharing.SID
		if rootID, err := d.sharing.DriveRootID(); err == nil {
			rootIDs = append(rootIDs, rootID)
		}
	}
	var res struct {
		Docs []struct {
			Found  bool          `json:"found"`
			Source ScopeDocument `json:"_source"`
		} `json:"docs"`
	}
	if err := client.Mget(ctx, ScopesIndex, ids, &res); err != nil {
		return nil, err
	}
	roots, err := loadDirs(inst, rootIDs)
	if err != nil {
		return nil, err
	}
	for i := range drives {
		if i < len(res.Docs) && res.Docs[i].Found && res.Docs[i].Source.OwnerInstanceID == inst.Domain {
			for _, key := range res.Docs[i].Source.SubtreeDirKeys {
				drives[i].keys[key] = true
			}
		}
		if rootID, err := drives[i].sharing.DriveRootID(); err == nil && roots[rootID] != nil {
			drives[i].rootPath = roots[rootID].Fullpath
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
			// Removed from the tree: moved out, trashed or deleted
			if d.keys[key] {
				add(d.sharing)
				continue
			}
			// Added to the tree: created or moved in
			if !change.Deleted && d.rootPath != "" && !isTrashed(folderPath) &&
				strings.HasPrefix(folderPath, d.rootPath+"/") {
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
			if !slices.Contains(deleted, change.DocID) {
				deleted = append(deleted, change.DocID)
			}
			continue
		}
		s, err := sharingFromChange(change)
		if err != nil || !s.Drive || !s.Owner {
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
