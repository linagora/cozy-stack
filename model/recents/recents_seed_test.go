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

	// The instance setup creates its default tree (Administrative, Photos…)
	// dated now: push the pre-existing directories in the past so that the
	// 25 files created below are the newest items of the instance.
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
