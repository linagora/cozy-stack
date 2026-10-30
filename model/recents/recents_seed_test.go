package recents_test

import (
	"fmt"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupSeedTest gives each sub-test its own instance: the seed is marked on
// the instance, so sharing one instance would make later sub-tests no-ops.
func setupSeedTest(t *testing.T) *instance.Instance {
	t.Helper()
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.URL = &url.URL{
		Scheme: "file",
		Host:   "localhost",
		Path:   t.TempDir(),
	}
	return testutils.NewSetup(t, t.Name()).GetTestInstance()
}

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

func createSeedDir(t *testing.T, inst *instance.Instance, name string) *vfs.DirDoc {
	t.Helper()
	parent, err := inst.VFS().DirByID(consts.RootDirID)
	require.NoError(t, err)
	dir, err := vfs.NewDirDocWithParent(name, parent, nil)
	require.NoError(t, err)
	require.NoError(t, inst.VFS().CreateDir(dir))
	return dir
}

func TestSeedCapsAt20WithRealDates(t *testing.T) {
	alice := setupSeedTest(t)

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
	require.NoError(t, recents.Seed(alice))

	docs := rows(t, alice)
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
		assert.Equal(t, recents.ResourceKey(alice, filesByIndex[expectedIndex]), doc.DocID,
			"the rows must be the 20 newest files, newest first")
	}
}

func TestSeedDirSeededWithCreationDate(t *testing.T) {
	alice := setupSeedTest(t)

	dir := createSeedDir(t, alice, "a-recent-folder")
	require.NoError(t, recents.Seed(alice))

	row := getRow(t, alice, recents.ResourceKey(alice, dir.ID()))
	assert.Equal(t, dir.CreatedAt.UTC().Format(time.RFC3339), row.LastEditedAt)
	assert.Equal(t, row.LastActivityAt, row.LastEditedAt)
	assert.Empty(t, row.LastViewedAt)
}

func TestSeedDirsPickedByCreationDate(t *testing.T) {
	alice := setupSeedTest(t)

	// A folder created last, whose updated_at is older than the one of many
	// old folders touched recently (renames, moves, content changes).
	fresh := createSeedDir(t, alice, "fresh-folder")
	fresh.UpdatedAt = time.Now().Add(-time.Hour)
	require.NoError(t, alice.VFS().UpdateDirDoc(fresh, fresh))

	old := time.Now().AddDate(-2, 0, 0)
	for i := 0; i < 25; i++ {
		dir := createSeedDir(t, alice, fmt.Sprintf("old-folder-%02d", i))
		dir.CreatedAt = old.Add(time.Duration(i) * time.Minute)
		dir.UpdatedAt = time.Now()
		require.NoError(t, alice.VFS().UpdateDirDoc(dir, dir))
	}

	require.NoError(t, recents.Seed(alice))
	row := getRow(t, alice, recents.ResourceKey(alice, fresh.ID()))
	assert.Equal(t, fresh.CreatedAt.UTC().Format(time.RFC3339), row.LastEditedAt)
}

func TestSeedSecondCallIsANoOp(t *testing.T) {
	alice := setupSeedTest(t)

	createSeedFile(t, alice, "once.txt", time.Now().Add(-time.Minute))
	require.NoError(t, recents.Seed(alice))
	before := rows(t, alice)
	require.NoError(t, recents.Seed(alice))
	assert.Equal(t, before, rows(t, alice), "a second seed must add nothing")
}

func TestSeedConcurrentMarkIsNotAFailure(t *testing.T) {
	alice := setupSeedTest(t)

	// Two concurrent first reads both seed and both store the marker.
	require.NoError(t, recents.MarkSeeded(alice))
	assert.NoError(t, recents.MarkSeeded(alice))
}

func TestSeedMarkerLivesInRecentsDatabase(t *testing.T) {
	alice := setupSeedTest(t)

	createSeedFile(t, alice, "marked.txt", time.Now().Add(-time.Minute))
	require.NoError(t, recents.Seed(alice))

	_, err := couchdb.GetLocal(alice, consts.FilesRecents, recents.SeededMarkerID)
	assert.NoError(t, err, "the marker is a _local document of the recents database")
	docs := rows(t, alice)
	require.NotEmpty(t, docs)
	for _, doc := range docs {
		assert.NotEmpty(t, doc.SourceLocator, "the marker must never be listed as a row")
	}
	var settings couchdb.JSONDoc
	err = couchdb.GetDoc(alice, consts.Settings, "io.cozy.settings.recents-seed", &settings)
	assert.True(t, couchdb.IsNotFoundError(err), "nothing is written in the settings")
}

func TestSeedMarkCreatesTheDatabase(t *testing.T) {
	alice := setupSeedTest(t)

	// A seed that found nothing to write has no recents database yet.
	_ = couchdb.DeleteDB(alice, consts.FilesRecents)
	require.NoError(t, recents.MarkSeeded(alice))

	_, err := couchdb.DBStatus(alice, consts.FilesRecents)
	assert.NoError(t, err, "marking the seed creates the recents database")
	_, err = couchdb.GetLocal(alice, consts.FilesRecents, recents.SeededMarkerID)
	assert.NoError(t, err)
}

func TestSeedTrashedFilesAreExcluded(t *testing.T) {
	alice := setupSeedTest(t)

	doc := createSeedFile(t, alice, "in-trash.txt", time.Now().Add(-time.Minute))
	doc.Trashed = true
	require.NoError(t, alice.VFS().UpdateFileDoc(doc, doc))

	require.NoError(t, recents.Seed(alice))
	for _, row := range rows(t, alice) {
		assert.NotEqual(t, recents.ResourceKey(alice, doc.ID()), row.DocID,
			"a trashed file must not be seeded")
	}
}

func TestSeedTrashedDirIsExcluded(t *testing.T) {
	alice := setupSeedTest(t)

	dir := createSeedDir(t, alice, "trashed-folder")
	trashed, err := vfs.TrashDir(alice.VFS(), dir)
	require.NoError(t, err)
	_ = trashed

	require.NoError(t, recents.Seed(alice))
	for _, row := range rows(t, alice) {
		assert.NotEqual(t, recents.ResourceKey(alice, dir.ID()), row.DocID,
			"a trashed directory must not be seeded")
	}
}

func TestSeedRealActivityUpdatesSeededRow(t *testing.T) {
	alice := setupSeedTest(t)

	doc := createSeedFile(t, alice, "touched-after-seed.txt", time.Now().Add(-time.Minute))
	require.NoError(t, recents.Seed(alice))
	key := recents.ResourceKey(alice, doc.ID())

	require.NoError(t, recents.RecordActivity(alice, alice, doc.ID(), recents.KindEdit))
	row := getRow(t, alice, key)
	assert.Equal(t, row.LastActivityAt, row.LastEditedAt,
		"a real edit after the seed updates the row normally")
}

// seededKeys returns the _id of the recents rows of the instance.
func seededKeys(t *testing.T, inst *instance.Instance) []string {
	t.Helper()
	var keys []string
	for _, row := range rows(t, inst) {
		keys = append(keys, row.DocID)
	}
	return keys
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

func TestSeedFutureDates(t *testing.T) {
	alice := setupSeedTest(t)

	// A client writing its local time as UTC dates an item up to 14 hours in
	// the future: it is seeded with its date. Beyond, it is not seeded.
	soon := createSeedFile(t, alice, "soon.txt", time.Now().Add(12*time.Hour))
	late := createSeedFile(t, alice, "late.txt", time.Now().Add(20*time.Hour))
	past := createSeedFile(t, alice, "past.txt", time.Now().Add(-time.Hour))
	lateDir := createSeedDir(t, alice, "late-folder")
	lateDir.CreatedAt = time.Now().Add(20 * time.Hour)
	lateDir.UpdatedAt = lateDir.CreatedAt
	require.NoError(t, alice.VFS().UpdateDirDoc(lateDir, lateDir))

	require.NoError(t, recents.Seed(alice))

	row := getRow(t, alice, recents.ResourceKey(alice, soon.ID()))
	assert.Equal(t, soon.UpdatedAt.UTC().Format(time.RFC3339), row.LastEditedAt,
		"a date less than 14 hours in the future is kept as it is")
	row = getRow(t, alice, recents.ResourceKey(alice, past.ID()))
	assert.Equal(t, past.UpdatedAt.UTC().Format(time.RFC3339), row.LastEditedAt)
	keys := seededKeys(t, alice)
	assert.NotContains(t, keys, recents.ResourceKey(alice, late.ID()),
		"a file dated more than 14 hours in the future must not be seeded")
	assert.NotContains(t, keys, recents.ResourceKey(alice, lateDir.ID()),
		"a folder dated more than 14 hours in the future must not be seeded")
}

func TestSeedFarFutureDatesDoNotTakePlaces(t *testing.T) {
	alice := setupSeedTest(t)

	future := time.Now().AddDate(1, 0, 0)
	for i := 0; i < 25; i++ {
		createSeedFile(t, alice, fmt.Sprintf("future-%02d.txt", i),
			future.Add(time.Duration(i)*time.Minute))
	}
	for i := 0; i < 25; i++ {
		dir := createSeedDir(t, alice, fmt.Sprintf("future-%02d", i))
		dir.CreatedAt = future.Add(time.Duration(i) * time.Minute)
		dir.UpdatedAt = dir.CreatedAt
		require.NoError(t, alice.VFS().UpdateDirDoc(dir, dir))
	}
	var past []string
	for i := 0; i < 3; i++ {
		doc := createSeedFile(t, alice, fmt.Sprintf("past-%02d.txt", i),
			time.Now().Add(-time.Duration(i+1)*time.Hour))
		past = append(past, recents.ResourceKey(alice, doc.ID()))
	}
	pastDir := createSeedDir(t, alice, "past-folder")
	past = append(past, recents.ResourceKey(alice, pastDir.ID()))

	require.NoError(t, recents.Seed(alice))
	keys := seededKeys(t, alice)
	for _, key := range past {
		assert.Contains(t, keys, key,
			"the items dated in the future must not take the places of the others")
	}
}

func TestSeedSystemAndMagicDirsAreExcluded(t *testing.T) {
	alice := setupSeedTest(t)

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

	require.NoError(t, recents.Seed(alice))

	keys := seededKeys(t, alice)
	for _, id := range []string{
		consts.RootDirID, consts.TrashDirID, sharedWithMe.ID(),
		notes.ID(), konnector.ID(), flagship.ID(), photos.ID(),
	} {
		assert.NotContains(t, keys, recents.ResourceKey(alice, id),
			"the folder %s was not created by the user", id)
	}
	assert.Contains(t, keys, recents.ResourceKey(alice, shared.ID()),
		"a folder shared by the user is seeded")
	assert.Contains(t, keys, recents.ResourceKey(alice, byDrive.ID()),
		"a folder created in Drive is seeded")
}

func TestSeedBackedUpPhotosAreExcluded(t *testing.T) {
	alice := setupSeedTest(t)

	backup, err := vfs.NewFileDoc("IMG_0001.jpg", consts.RootDirID, -1, nil,
		"image/jpeg", "image", time.Now().Add(-time.Minute), false, false, false, nil)
	require.NoError(t, err)
	backup.Metadata = vfs.Metadata{"backupDeviceIds": []interface{}{"device-1"}}
	f, err := alice.VFS().CreateFile(backup, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	edited := createSeedFile(t, alice, "edited.txt", time.Now().Add(-time.Hour))

	require.NoError(t, recents.Seed(alice))

	keys := seededKeys(t, alice)
	assert.NotContains(t, keys, recents.ResourceKey(alice, backup.ID()),
		"a photo saved by the backup of the flagship app must not be seeded")
	assert.Contains(t, keys, recents.ResourceKey(alice, edited.ID()))
}

func TestForgetSeedSeedsAgain(t *testing.T) {
	alice := setupSeedTest(t)

	// Without a marker, there is nothing to forget.
	require.NoError(t, recents.ForgetSeed(alice))

	createSeedFile(t, alice, "before.txt", time.Now().Add(-time.Hour))
	require.NoError(t, recents.Seed(alice))

	require.NoError(t, recents.ForgetSeed(alice))
	_, err := couchdb.GetLocal(alice, consts.FilesRecents, recents.SeededMarkerID)
	assert.True(t, couchdb.IsNotFoundError(err), "the marker is removed")

	after := createSeedFile(t, alice, "after.txt", time.Now().Add(-time.Minute))
	require.NoError(t, recents.Seed(alice))
	assert.Contains(t, seededKeys(t, alice), recents.ResourceKey(alice, after.ID()),
		"a forgotten seed runs again")
	_, err = couchdb.GetLocal(alice, consts.FilesRecents, recents.SeededMarkerID)
	assert.NoError(t, err, "the seed is marked again")
}
