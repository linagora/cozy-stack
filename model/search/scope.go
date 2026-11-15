package search

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/search/purge"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/opensearch"
)

// maxScopeKeys is the number of folder keys of a scope above which a warning
// is logged, as each search sends them all in its terms filter.
const maxScopeKeys = 50000

// Names of the search indices, before the configured prefix.
const (
	FilesIndex  = purge.FilesIndex
	ScopesIndex = purge.ScopesIndex
)

// subtreePageSize is the number of folders loaded per CouchDB request when
// listing the folders of a shared drive (a variable for the tests).
var subtreePageSize = 10000

// ScopeDocument is a shared drive in the cozy_share_scopes_v1 index: who can
// see it, and the keys of all its folders.
type ScopeDocument struct {
	SharingID              string   `json:"sharing_id"`
	OwnerInstanceID        string   `json:"owner_instance_id"`
	RootDirID              string   `json:"root_dir_id"`
	ActiveMemberPrincipals []string `json:"active_member_principals"`
	SubtreeDirKeys         []string `json:"subtree_dir_keys"`
}

// NewScopeDocument builds the scope document of a shared drive, on the
// instance of its owner. It returns false when the drive must not have a scope
// document: not an active drive owned by the instance, no recipient has
// accepted it, or its root is missing or in the trash.
func NewScopeDocument(inst *instance.Instance, s *sharing.Sharing) (*ScopeDocument, bool, error) {
	if !s.Drive || !s.Owner || !s.Active {
		return nil, false, nil
	}
	principals := activePrincipals(s)
	if len(principals) == 0 {
		return nil, false, nil
	}
	rootID, err := s.DriveRootID()
	if err != nil {
		return nil, false, nil
	}
	root, err := inst.VFS().DirByID(rootID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if strings.HasPrefix(root.Fullpath, vfs.TrashDirName+"/") {
		return nil, false, nil
	}
	ids, err := subdirIDs(inst, root.Fullpath)
	if err != nil {
		return nil, false, err
	}
	if len(ids)+1 > maxScopeKeys {
		inst.Logger().WithNamespace("search").
			Warnf("the scope of the drive %s has %d folder keys", s.SID, len(ids)+1)
	}
	keys := make([]string, 0, len(ids)+1)
	keys = append(keys, Key(inst.Domain, rootID))
	for _, id := range ids {
		keys = append(keys, Key(inst.Domain, id))
	}
	return &ScopeDocument{
		SharingID:              s.SID,
		OwnerInstanceID:        inst.Domain,
		RootDirID:              rootID,
		ActiveMemberPrincipals: principals,
		SubtreeDirKeys:         keys,
	}, true, nil
}

// activePrincipals returns the domains of the recipients who accepted the
// sharing. Members[0] is the owner.
func activePrincipals(s *sharing.Sharing) []string {
	var principals []string
	for i, m := range s.Members {
		if i == 0 || m.Status != sharing.MemberStatusReady {
			continue
		}
		if host := m.InstanceHost(); host != "" {
			principals = append(principals, host)
		}
	}
	return principals
}

// subdirIDs returns the ids of all the folders under the given path, with
// paged queries on the dir-by-path index (no recursive walk). The folders
// under a skipped folder are left out.
func subdirIDs(db *instance.Instance, dirPath string) ([]string, error) {
	sel := mango.And(
		mango.Gt("path", dirPath+"/"),
		mango.Lt("path", dirPath+"/￰"),
		mango.Equal("type", consts.DirType),
	)
	var ids []string
	bookmark := ""
	for {
		req := &couchdb.FindRequest{
			UseIndex: "dir-by-path",
			Selector: sel,
			Fields:   []string{"_id", "path"},
			Limit:    subtreePageSize,
			Bookmark: bookmark,
		}
		var docs []couchdb.JSONDoc
		res, err := couchdb.FindDocsRaw(db, consts.Files, req, &docs)
		if err != nil {
			return nil, err
		}
		for _, doc := range docs {
			if p, _ := doc.M["path"].(string); !inSkippedTree(path.Dir(p)) {
				ids = append(ids, doc.ID())
			}
		}
		if len(docs) < subtreePageSize || res.Bookmark == "" {
			return ids, nil
		}
		bookmark = res.Bookmark
	}
}

// UpsertScope writes the scope document of a shared drive.
func UpsertScope(ctx context.Context, client *opensearch.Client, doc *ScopeDocument) error {
	action, err := json.Marshal(map[string]any{"index": map[string]string{"_id": doc.SharingID}})
	if err != nil {
		return err
	}
	source, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	ndjson := append(append(append(action, '\n'), source...), '\n')
	return client.Bulk(ctx, ScopesIndex, ndjson)
}

// DeleteScope removes the scope document of a shared drive, if it belongs to
// the instance: a sharing has the same id on the instances of its owner and
// of its recipients, and only the owner's one has a scope document. Deleting
// a missing document is not an error.
func DeleteScope(ctx context.Context, client *opensearch.Client, domain, sharingID string) error {
	var res struct {
		Docs []struct {
			Found  bool          `json:"found"`
			Source ScopeDocument `json:"_source"`
		} `json:"docs"`
	}
	if err := client.Mget(ctx, ScopesIndex, []string{sharingID}, &res); err != nil {
		return err
	}
	if len(res.Docs) == 0 || !res.Docs[0].Found || res.Docs[0].Source.OwnerInstanceID != domain {
		return nil
	}
	action, err := json.Marshal(map[string]any{"delete": map[string]string{"_id": sharingID}})
	if err != nil {
		return err
	}
	err = client.Bulk(ctx, ScopesIndex, append(action, '\n'))
	var osErr *opensearch.Error
	if errors.As(err, &osErr) && osErr.Status == 404 {
		return nil
	}
	return err
}
