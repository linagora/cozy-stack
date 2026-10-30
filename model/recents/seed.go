package recents

import (
	"sort"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/metrics"
)

// seedLimit is the maximum number of rows the seed writes, files and folders
// merged.
const seedLimit = 20

// Seed fills the recents of the instance with up to 20 rows for the files
// most recently modified and the folders created that the user can see, so
// that switching to the server-side recents never shows an empty page. A
// seeded file carries its last modification date, a seeded folder its
// creation date: never the seed time. The rows then live like any other row,
// updated by the real activity. The call is idempotent: a marker in the
// recents database makes subsequent calls no-ops, and concurrent calls cannot
// duplicate rows as the upsert keys them by their resource key.
func Seed(inst *instance.Instance) error {
	marked, err := isSeeded(inst)
	if err != nil {
		// Best effort: an unreadable marker means no seed, not an error for
		// the caller.
		metrics.RecentsSeedFailures.Inc()
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot read the recents seed marker: %s", err)
		return nil
	}
	if marked {
		return nil
	}

	if err := seedFiles(inst); err != nil {
		// Best effort: a failed seed shows an empty page, not an error.
		metrics.RecentsSeedFailures.Inc()
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot seed the recents: %s", err)
		return nil
	}
	if err := markSeeded(inst); err != nil {
		metrics.RecentsSeedFailures.Inc()
		return err
	}
	return nil
}

// seededCacheTTL is how long the "seeded" state of an instance is cached, so
// that the marker is not read on every read of the recents.
const seededCacheTTL = time.Hour

func seededCacheKey(inst *instance.Instance) string {
	return "recents-seeded:" + inst.DocID
}

// seededMarkerID is the id of the _local document of the recents database
// that marks it as seeded. CouchDB never returns _local documents from
// _all_docs, _changes or a find request: the marker is never a recents row,
// and it is neither exported nor replicated.
const seededMarkerID = "seeded"

// isSeeded reports whether the recents of the instance were already seeded.
func isSeeded(inst *instance.Instance) (bool, error) {
	cache := config.GetConfig().CacheStorage
	if _, ok := cache.Get(seededCacheKey(inst)); ok {
		return true, nil
	}
	_, err := couchdb.GetLocal(inst, consts.FilesRecents, seededMarkerID)
	if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cache.Set(seededCacheKey(inst), []byte("1"), seededCacheTTL)
	return true, nil
}

// markSeeded stores the marker so that the seed runs only once per instance.
// The database is created when the seed found nothing to write. A conflict
// means a concurrent seed stored the marker first, which is a success.
func markSeeded(inst *instance.Instance) error {
	if err := couchdb.EnsureDBExist(inst, consts.FilesRecents); err != nil {
		return err
	}
	err := couchdb.PutLocal(inst, consts.FilesRecents, seededMarkerID,
		map[string]interface{}{"seeded_at": time.Now().UTC().Format(time.RFC3339)})
	if err != nil && !couchdb.IsConflictError(err) {
		return err
	}
	config.GetConfig().CacheStorage.Set(seededCacheKey(inst), []byte("1"), seededCacheTTL)
	return nil
}

// seedItem is the projection of a file or directory document needed to seed
// its recents row.
type seedItem struct {
	DocID     string    `json:"_id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Path      string    `json:"path"`
}

// seedDate is the date the recents row of the item will carry: the last
// modification for a file, the creation for a folder — a folder's updated_at
// moves on every rename or move of its content, which is not an edit.
func seedDate(item *seedItem) time.Time {
	if item.Type == consts.DirType {
		return item.CreatedAt
	}
	return item.UpdatedAt
}

// seedFiles writes the recents rows from the local io.cozy.files of the
// instance, which materializes every item the user can see, received shares
// included. Items of other stacks are not in this database, coherent with the
// scope of the recents.
func seedFiles(inst *instance.Instance) error {
	items, err := findSeedFiles(inst)
	if err != nil {
		return err
	}
	dirs, err := findSeedDirs(inst)
	if err != nil {
		return err
	}
	items = append(items, dirs...)

	// The two types are ranked on the date their row will carry, and only the
	// seedLimit most recent are seeded.
	sort.Slice(items, func(i, j int) bool {
		return seedDate(items[i]).After(seedDate(items[j]))
	})
	if len(items) > seedLimit {
		items = items[:seedLimit]
	}
	for _, item := range items {
		if err := recordActivity(inst, inst, item.DocID, KindEdit, seedDate(item)); err != nil {
			return err
		}
	}
	return nil
}

// findSeedFiles returns the most recently modified non-trashed files. The
// trashed boolean is only set on files, so the selector cannot match
// directories.
func findSeedFiles(inst *instance.Instance) ([]*seedItem, error) {
	var items []*seedItem
	req := &couchdb.FindRequest{
		UseIndex: "by-updated-at-trashed",
		Selector: mango.And(
			mango.In("type", []interface{}{consts.FileType}),
			mango.Equal("trashed", false),
			mango.Gt("updated_at", nil),
		),
		Sort: mango.SortBy{
			{Field: "updated_at", Direction: mango.Desc},
		},
		Limit:  seedLimit,
		Fields: []string{"_id", "type", "created_at", "updated_at"},
	}
	return items, findSeedDocs(inst, req, &items)
}

// findSeedDirs returns the most recently created directories, the date their
// row will carry. Directories have no trashed boolean: a trashed directory is
// moved inside the trash directory, recognized by its path. The two system
// directories are not something the user created, they are not seeded: two
// more are asked to make up for them.
func findSeedDirs(inst *instance.Instance) ([]*seedItem, error) {
	var items []*seedItem
	req := &couchdb.FindRequest{
		UseIndex: "by-type-created-at",
		Selector: mango.And(
			mango.Equal("type", consts.DirType),
			mango.Gt("created_at", nil),
		),
		Sort: mango.SortBy{
			{Field: "created_at", Direction: mango.Desc},
		},
		Limit:  seedLimit + 2,
		Fields: []string{"_id", "type", "created_at", "updated_at", "path"},
	}
	if err := findSeedDocs(inst, req, &items); err != nil {
		return nil, err
	}
	kept := items[:0]
	for _, item := range items {
		if item.DocID == consts.RootDirID || item.DocID == consts.TrashDirID {
			continue
		}
		if strings.HasPrefix(item.Path, vfs.TrashDirName+"/") {
			continue
		}
		kept = append(kept, item)
	}
	return kept, nil
}

// findSeedDocs runs a seed find request, treating a missing database as
// nothing to seed.
func findSeedDocs(inst *instance.Instance, req *couchdb.FindRequest, items *[]*seedItem) error {
	err := couchdb.FindDocs(inst, consts.Files, req, items)
	if couchdb.IsNoDatabaseError(err) {
		return nil
	}
	return err
}
