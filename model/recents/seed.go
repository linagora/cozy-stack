package recents

import (
	"regexp"
	"sort"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
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
// creation date: never the seed time. The system and magic folders and the
// photo backup are not something the user did, they are not seeded. The rows
// then live like any other row, updated by the real activity. The call is
// idempotent: a marker in the recents database makes subsequent calls no-ops,
// and concurrent calls cannot duplicate rows as the upsert keys them by their
// resource key.
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

// seededMarkerID is the id of the _local document of the recents database
// that marks it as seeded. CouchDB never returns _local documents from
// _all_docs, _changes or a find request: the marker is never a recents row,
// and it is neither exported nor replicated.
const seededMarkerID = "seeded"

// isSeeded reports whether the recents of the instance were already seeded.
func isSeeded(inst *instance.Instance) (bool, error) {
	_, err := couchdb.GetLocal(inst, consts.FilesRecents, seededMarkerID)
	if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
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
	return nil
}

// ForgetSeed removes the seed marker of the instance, so that the next read
// seeds its recents again. An import replaces the files of the instance: a
// marker stored by a seed that ran on the replaced files, while the import
// was starting, must not prevent it.
func ForgetSeed(inst *instance.Instance) error {
	err := couchdb.DeleteLocal(inst, consts.FilesRecents, seededMarkerID)
	if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
		return nil
	}
	return err
}

// seedItem is the projection of a file or directory document needed to seed
// its recents row.
type seedItem struct {
	DocID     string    `json:"_id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// seedDate is the date the recents row of the item will carry: the last
// modification for a file, the creation for a folder — cozy-desktop sets the
// updated_at of a folder to its local modification time, which moves every
// time an entry is added, removed or renamed inside, and that is not an edit.
func seedDate(item *seedItem) time.Time {
	if item.Type == consts.DirType {
		return item.CreatedAt
	}
	return item.UpdatedAt
}

// maxClockOffset is how far in the future the date of a seeded item can be.
// The dates of the files and folders are given by the clients, and a client
// can write its local time as if it were UTC: 14 hours is the largest offset
// of a time zone (UTC+14), so such a date is never further in the future. An
// item dated beyond is not seeded, as its row would stay on top of the recents
// until that date.
const maxClockOffset = 14 * time.Hour

// seedQueryBound returns the upper bound of the dates in the seed find
// requests, so that the items dated far in the future do not take the places
// of the other ones. Mango compares the dates as strings: a date stored with
// its time zone offset, or a local time written as UTC, reads at most
// maxClockOffset ahead. The exact check is made on the parsed dates, and an
// item beyond the bound is left out by the requests, without a warning.
func seedQueryBound(now time.Time) time.Time {
	return now.UTC().Add(maxClockOffset)
}

// keepSeedDates removes the items dated too far in the future. The items
// dated in the future but within maxClockOffset are kept with their date.
func keepSeedDates(inst *instance.Instance, items []*seedItem, now time.Time) []*seedItem {
	log := inst.Logger().WithNamespace("recents")
	kept := items[:0]
	for _, item := range items {
		date := seedDate(item)
		if date.After(now.Add(maxClockOffset)) {
			log.Warnf("Not seeding %s, dated too far in the future: %s",
				item.DocID, date.Format(time.RFC3339))
			continue
		}
		if date.After(now) {
			log.Warnf("Seeding %s with a date in the future: %s",
				item.DocID, date.Format(time.RFC3339))
		}
		kept = append(kept, item)
	}
	return kept
}

// seedFiles writes the recents rows from the local io.cozy.files of the
// instance, which materializes every item the user can see, received shares
// included. Items of other stacks are not in this database, coherent with the
// scope of the recents.
func seedFiles(inst *instance.Instance) error {
	now := time.Now()
	items, err := findSeedFiles(inst, now)
	if err != nil {
		return err
	}
	dirs, err := findSeedDirs(inst, now)
	if err != nil {
		return err
	}
	items = keepSeedDates(inst, append(items, dirs...), now)

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

// backupDeviceIDsField is the metadata that the flagship app sets on the files
// of a photo backup.
const backupDeviceIDsField = "metadata.backupDeviceIds"

// findSeedFiles returns the most recently modified non-trashed files. The
// trashed boolean is only set on files, so the selector cannot match
// directories. The photos saved by the backup of the flagship app are not
// modified by the user, they are left out.
func findSeedFiles(inst *instance.Instance, now time.Time) ([]*seedItem, error) {
	var items []*seedItem
	req := &couchdb.FindRequest{
		UseIndex: "by-updated-at-trashed",
		Selector: mango.And(
			mango.In("type", []interface{}{consts.FileType}),
			mango.Equal("trashed", false),
			mango.Lte("updated_at", seedQueryBound(now)),
			mango.NotExists(backupDeviceIDsField),
		),
		Sort: mango.SortBy{
			{Field: "updated_at", Direction: mango.Desc},
		},
		Limit:  seedLimit,
		Fields: []string{"_id", "type", "created_at", "updated_at"},
	}
	err := findSeedDocs(inst, req, &items)
	return items, err
}

// flagshipSlug is the cozyMetadata.createdByApp of the documents created by
// the flagship app with its own OAuth token.
const flagshipSlug = "cozy-flagship"

// systemDirIDs matches the fixed ids of the system directories: the root, the
// trash, the shared with me and no longer shared directories, and the
// container of the shared drives.
var systemDirIDs = "^" + regexp.QuoteMeta("io.cozy.files.")

// trashedPaths matches the paths of the directories inside the trash.
var trashedPaths = "^" + regexp.QuoteMeta(vfs.TrashDirName+"/")

// findSeedDirs returns the most recently created directories, the date their
// row will carry, leaving out the directories that the user did not create:
//   - the system directories;
//   - the trashed ones, as directories have no trashed boolean: a trashed
//     directory is moved inside the trash directory, recognized by its path;
//   - the magic folders, referenced by an app (Administrative, Photos, Notes,
//     the photo backup…) or a konnector;
//   - the folders created by the flagship app, which creates folders only for
//     its own automatic writes (photo backup tree, konnector folders, logs).
//
// A negation does not match a document without the field in Mango, hence the
// explicit branches for a missing field.
func findSeedDirs(inst *instance.Instance, now time.Time) ([]*seedItem, error) {
	var items []*seedItem
	req := &couchdb.FindRequest{
		UseIndex: "by-type-created-at",
		Selector: mango.And(
			mango.Equal("type", consts.DirType),
			mango.Lte("created_at", seedQueryBound(now)),
			mango.Map{"_id": mango.Map{"$not": mango.Map{"$regex": systemDirIDs}}},
			mango.Map{"path": mango.Map{"$not": mango.Map{"$regex": trashedPaths}}},
			mango.Or(
				mango.NotExists("referenced_by"),
				mango.Map{"referenced_by": mango.Map{"$not": mango.Map{"$elemMatch": mango.Map{
					"type": mango.Map{"$in": []string{consts.Apps, consts.Konnectors}},
				}}}},
			),
			mango.Or(
				mango.NotExists("cozyMetadata.createdByApp"),
				mango.NotEqual("cozyMetadata.createdByApp", flagshipSlug),
			),
		),
		Sort: mango.SortBy{
			{Field: "created_at", Direction: mango.Desc},
		},
		Limit:  seedLimit,
		Fields: []string{"_id", "type", "created_at", "updated_at"},
	}
	err := findSeedDocs(inst, req, &items)
	return items, err
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
