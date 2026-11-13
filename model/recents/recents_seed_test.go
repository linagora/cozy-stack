package recents_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The seed is marked on the instance: each test has its own instance, as
// sharing one would make the later seeds no-ops.

// createSeedFile creates a file whose updated_at is pushed to the given time,
// so that the seed query has distinct, controlled modification dates.
func createSeedFile(t *testing.T, inst *instance.Instance, name string, modifiedAt time.Time) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
		"text/plain", "text", modifiedAt, false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	doc.UpdatedAt = modifiedAt
	require.NoError(t, inst.VFS().UpdateFileDoc(doc, doc))
	return doc
}

// createSeedDirAt creates a directory at the root, with the given creation
// date.
func createSeedDirAt(t *testing.T, inst *instance.Instance, name string, createdAt time.Time) *vfs.DirDoc {
	t.Helper()
	dir := createDir(t, inst, name)
	dir.CreatedAt = createdAt
	dir.UpdatedAt = createdAt
	require.NoError(t, inst.VFS().UpdateDirDoc(dir, dir))
	return dir
}

// createSeedDirDoc creates a directory at the root after letting the test set
// its references or metadata.
func createSeedDirDoc(t *testing.T, inst *instance.Instance, name string, set func(dir *vfs.DirDoc)) *vfs.DirDoc {
	t.Helper()
	dir, err := vfs.NewDirDocWithPath(name, consts.RootDirID, "/", nil)
	require.NoError(t, err)
	set(dir)
	require.NoError(t, inst.VFS().CreateDir(dir))
	return dir
}

// seededKeys returns the _id of the recents rows of the instance.
func seededKeys(t *testing.T, inst *instance.Instance) []string {
	t.Helper()
	var keys []string
	for _, row := range allRows(t, inst) {
		keys = append(keys, row.DocID)
	}
	return keys
}

func TestSeed(t *testing.T) {
	alice, _ := setupRecordTest(t)

	file := createFile(t, alice, "metadata.txt")
	dir := createDir(t, alice, "Metadata")

	recents.Seed(alice)

	fileRow := getRow(t, alice, file.ID())
	assert.Equal(t, "metadata.txt", fileRow.Name)
	assert.Equal(t, consts.FileType, fileRow.Type)
	assert.Equal(t, "text/plain", fileRow.Mime)
	assert.Equal(t, "text", fileRow.Class)
	assert.Equal(t, "5", fileRow.Size)
	assert.Equal(t, consts.RootDirID, fileRow.DirID)
	assert.Equal(t, "/metadata.txt", fileRow.Path)
	assert.Equal(t, file.UpdatedAt.Format(time.RFC3339Nano), fileRow.UpdatedAt)
	assert.Equal(t, recents.SchemaVersion, fileRow.SchemaVersion)
	assert.Empty(t, fileRow.DriveID)
	assert.Empty(t, fileRow.OwnerDomain)

	dirRow := getRow(t, alice, dir.ID())
	assert.Equal(t, "Metadata", dirRow.Name)
	assert.Equal(t, consts.DirType, dirRow.Type)
	assert.Equal(t, "/Metadata", dirRow.Path)
	assert.Empty(t, dirRow.Mime)
	assert.Empty(t, dirRow.Class)
	assert.Empty(t, dirRow.Size)
	// A folder carries its creation date, and a seeded row has no view.
	assert.Equal(t, stamp(dir.CreatedAt), dirRow.LastEditedAt)
	assert.Equal(t, dirRow.LastActivityAt, dirRow.LastEditedAt)
	assert.Empty(t, dirRow.LastViewedAt)

	_, err := couchdb.GetLocal(alice, consts.FilesRecents, recents.SeededMarkerID)
	assert.NoError(t, err, "the marker is a _local document of the recents database")
	rows := allRows(t, alice)
	assert.NotContains(t, seededKeys(t, alice), recents.SeededMarkerID,
		"the marker must never be listed as a row")

	createFile(t, alice, "after-the-seed.txt")
	recents.Seed(alice)
	assert.Equal(t, rows, allRows(t, alice), "a second seed must add nothing")

	require.NoError(t, recents.RecordActivity(alice, alice, nil, file, recents.KindEdit, nil))
	fileRow = getRow(t, alice, file.ID())
	assert.Equal(t, fileRow.LastActivityAt, fileRow.LastEditedAt,
		"a real edit after the seed updates the row normally")
}

func TestSeedCapsAt20WithRealDates(t *testing.T) {
	alice, _ := setupRecordTest(t)

	// The seed leaves out the default tree of the instance (Administrative and
	// Photos are referenced by an app), but the test must not depend on it:
	// push the pre-existing directories in the past so that the 25 files
	// created below are the newest items of the instance whatever the tree.
	old := time.Now().AddDate(0, -2, 0)
	var dirs []*vfs.DirDoc
	require.NoError(t, couchdb.FindDocs(alice, consts.Files,
		&couchdb.FindRequest{
			UseIndex: "by-type-created-at",
			Selector: mango.And(
				mango.Equal("type", consts.DirType),
				mango.Gt("created_at", nil),
			),
		}, &dirs))
	for _, dir := range dirs {
		if dir.ID() == consts.RootDirID || dir.ID() == consts.TrashDirID {
			continue
		}
		dir.CreatedAt = old.Add(-24 * time.Hour)
		dir.UpdatedAt = dir.CreatedAt
		require.NoError(t, alice.VFS().UpdateDirDoc(dir, dir))
	}

	// 25 files, each modified at a distinct date in the past.
	filesByIndex := make(map[int]string, 25)
	for i := 0; i < 25; i++ {
		doc := createSeedFile(t, alice, fmt.Sprintf("file-%02d.txt", i),
			old.Add(time.Duration(i)*time.Minute))
		filesByIndex[i] = doc.ID()
	}

	before := time.Now()
	recents.Seed(alice)

	docs := allRows(t, alice)
	require.Len(t, docs, 20, "the seed must be capped at 20 rows")

	// No seeded row is stamped with the seed time: every date comes from the
	// item itself, before the seed ran.
	for _, doc := range docs {
		assert.True(t, doc.LastActivityAt < before.UTC().Format(time.RFC3339),
			"seeded row %s carries the seed time: %s", doc.DocID, doc.LastActivityAt)
		assert.True(t, doc.LastActivityAt >= old.UTC().Format(time.RFC3339))
		assert.Empty(t, doc.LastViewedAt, "a seeded row has no view")
		assert.Equal(t, doc.LastEditedAt, doc.LastActivityAt)
	}

	// The rows carry the file IDs and their real modification dates. The row
	// order in _all_docs is by _id, so sort by date for the recency check.
	sorted := make([]recents.FileDirActivity, len(docs))
	copy(sorted, docs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].LastActivityAt > sorted[j].LastActivityAt
	})
	for i, doc := range sorted {
		expectedIndex := 24 - i
		assert.Equal(t, filesByIndex[expectedIndex], doc.DocID,
			"the rows must be the 20 newest files, newest first")
	}
}

func TestSeedDirsPickedByCreationDate(t *testing.T) {
	alice, _ := setupRecordTest(t)

	// A folder created last, whose updated_at is older than the one of many
	// old folders touched recently (renames, moves, content changes).
	fresh := createDir(t, alice, "fresh-folder")
	fresh.UpdatedAt = time.Now().Add(-time.Hour)
	require.NoError(t, alice.VFS().UpdateDirDoc(fresh, fresh))

	old := time.Now().AddDate(-2, 0, 0)
	for i := 0; i < 25; i++ {
		dir := createDir(t, alice, fmt.Sprintf("old-folder-%02d", i))
		dir.CreatedAt = old.Add(time.Duration(i) * time.Minute)
		dir.UpdatedAt = time.Now()
		require.NoError(t, alice.VFS().UpdateDirDoc(dir, dir))
	}

	recents.Seed(alice)
	row := getRow(t, alice, fresh.ID())
	assert.Equal(t, stamp(fresh.CreatedAt), row.LastEditedAt)
}

func TestSeedMarker(t *testing.T) {
	alice, _ := setupRecordTest(t)

	// A seed that found nothing to write has no recents database yet.
	_ = couchdb.DeleteDB(alice, consts.FilesRecents)
	require.NoError(t, recents.MarkSeeded(alice))

	_, err := couchdb.DBStatus(alice, consts.FilesRecents)
	assert.NoError(t, err, "marking the seed creates the recents database")
	_, err = couchdb.GetLocal(alice, consts.FilesRecents, recents.SeededMarkerID)
	assert.NoError(t, err)

	// Two concurrent first reads both seed and both store the marker.
	assert.NoError(t, recents.MarkSeeded(alice))
}

func TestSeedFutureDates(t *testing.T) {
	alice, _ := setupRecordTest(t)

	// More items dated far in the future than the seed takes: they must not
	// take the places of the others.
	future := time.Now().AddDate(1, 0, 0)
	for i := 0; i < 25; i++ {
		at := future.Add(time.Duration(i) * time.Minute)
		createSeedFile(t, alice, fmt.Sprintf("future-%02d.txt", i), at)
		createSeedDirAt(t, alice, fmt.Sprintf("future-%02d", i), at)
	}
	// A client writing its local time as UTC dates an item up to 14 hours in
	// the future: it is seeded with its date. Beyond, it is not seeded.
	soon := createSeedFile(t, alice, "soon.txt", time.Now().Add(12*time.Hour))
	late := createSeedFile(t, alice, "late.txt", time.Now().Add(20*time.Hour))
	lateDir := createSeedDirAt(t, alice, "late-folder", time.Now().Add(20*time.Hour))
	kept := []*vfs.FileDoc{soon}
	for i := 0; i < 3; i++ {
		kept = append(kept, createSeedFile(t, alice, fmt.Sprintf("past-%02d.txt", i),
			time.Now().Add(-time.Duration(i+1)*time.Hour)))
	}
	pastDir := createDir(t, alice, "past-folder")

	recents.Seed(alice)

	for _, doc := range kept {
		row := getRow(t, alice, doc.ID())
		assert.Equal(t, stamp(doc.UpdatedAt), row.LastEditedAt,
			"%s is seeded with its own date", doc.DocName)
	}
	keys := seededKeys(t, alice)
	assert.Contains(t, keys, pastDir.ID())
	assert.NotContains(t, keys, late.ID(),
		"a file dated more than 14 hours in the future must not be seeded")
	assert.NotContains(t, keys, lateDir.ID(),
		"a folder dated more than 14 hours in the future must not be seeded")
}

func TestSeedLeavesOutWhatTheUserDidNotDo(t *testing.T) {
	alice, _ := setupRecordTest(t)

	trashedFile := createSeedFile(t, alice, "in-trash.txt", time.Now().Add(-time.Minute))
	trashedFile.Trashed = true
	require.NoError(t, alice.VFS().UpdateFileDoc(trashedFile, trashedFile))
	trashedDir := createDir(t, alice, "trashed-folder")
	_, err := vfs.TrashDir(alice.VFS(), trashedDir)
	require.NoError(t, err)

	backup, err := vfs.NewFileDoc("IMG_0001.jpg", consts.RootDirID, -1, nil,
		"image/jpeg", "image", time.Now().Add(-time.Minute), false, false, false, nil)
	require.NoError(t, err)
	backup.Metadata = vfs.Metadata{"backupDeviceIds": []interface{}{"device-1"}}
	f, err := alice.VFS().CreateFile(backup, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	edited := createSeedFile(t, alice, "edited.txt", time.Now().Add(-time.Hour))

	sharedWithMe := createSeedDirDoc(t, alice, "Shared with me", func(dir *vfs.DirDoc) {
		dir.DocID = consts.SharedWithMeDirID
	})
	notes := createSeedDirDoc(t, alice, "Notes", func(dir *vfs.DirDoc) {
		dir.AddReferencedBy(couchdb.DocReference{Type: consts.Apps, ID: "io.cozy.apps/notes"})
	})
	konnector := createSeedDirDoc(t, alice, "Konnector", func(dir *vfs.DirDoc) {
		dir.AddReferencedBy(couchdb.DocReference{Type: consts.Konnectors, ID: "io.cozy.konnectors/dummy"})
	})
	flagship := createSeedDirDoc(t, alice, "Backed up from my mobile", func(dir *vfs.DirDoc) {
		dir.CozyMetadata = vfs.NewCozyMetadata(alice.PageURL("/", nil))
		dir.CozyMetadata.CreatedByApp = "cozy-flagship"
	})
	shared := createSeedDirDoc(t, alice, "Shared by me", func(dir *vfs.DirDoc) {
		dir.AddReferencedBy(couchdb.DocReference{Type: consts.Sharings, ID: "a-sharing-id"})
	})
	byDrive := createSeedDirDoc(t, alice, "Created in Drive", func(dir *vfs.DirDoc) {
		dir.CozyMetadata = vfs.NewCozyMetadata(alice.PageURL("/", nil))
		dir.CozyMetadata.CreatedByApp = "drive"
	})
	photos, err := alice.VFS().DirByPath("/" + alice.Translate("Tree Photos"))
	require.NoError(t, err, "the instance is created with a Photos folder")

	recents.Seed(alice)

	keys := seededKeys(t, alice)
	for what, id := range map[string]string{
		"the root":                 consts.RootDirID,
		"the trash":                consts.TrashDirID,
		"a system folder":          sharedWithMe.ID(),
		"the folder of an app":     notes.ID(),
		"a konnector folder":       konnector.ID(),
		"a folder of the flagship": flagship.ID(),
		"the Photos folder":        photos.ID(),
		"a trashed file":           trashedFile.ID(),
		"a trashed folder":         trashedDir.ID(),
		"a photo of the backup":    backup.ID(),
	} {
		assert.NotContains(t, keys, id, "%s must not be seeded", what)
	}
	for what, id := range map[string]string{
		"a file edited by the user":   edited.ID(),
		"a folder shared by the user": shared.ID(),
		"a folder created in Drive":   byDrive.ID(),
	} {
		assert.Contains(t, keys, id, "%s is seeded", what)
	}
}
