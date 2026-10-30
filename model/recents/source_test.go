package recents_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sourceRow returns a row of an item of a source instance.
func sourceRow(src *instance.Instance, fileID string) recents.FileDirActivity {
	return recents.FileDirActivity{
		DocID:         recents.ResourceKey(src, fileID),
		SourceLocator: src.Domain + "/" + fileID,
	}
}

// updateStoredInstance applies a change to the instance document in CouchDB.
func updateStoredInstance(t *testing.T, inst *instance.Instance, change func(*instance.Instance)) {
	t.Helper()
	var stored instance.Instance
	require.NoError(t, couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Instances, inst.DocID, &stored))
	change(&stored)
	require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, &stored))
}

// markMoved marks an instance as moved, with the target of the move in its
// settings when movedTo is not empty, as the end of a move does.
func markMoved(t *testing.T, inst *instance.Instance, movedTo string) {
	t.Helper()
	if movedTo != "" {
		doc, err := inst.SettingsDocument()
		require.NoError(t, err)
		doc.M["moved_to"] = movedTo
		require.NoError(t, couchdb.UpdateDoc(inst, doc))
	}
	updateStoredInstance(t, inst, func(stored *instance.Instance) { stored.Moved = true })
}

func resolveSource(user *instance.Instance, row recents.FileDirActivity) *recents.Source {
	sources := recents.NewSources(user)
	sources.Resolve([]recents.FileDirActivity{row})
	instID, _, _ := recents.ParseResourceKey(row.DocID)
	return sources.Get(instID)
}

func TestSourcesOwnRowsAreLive(t *testing.T) {
	alice, _ := setupRecentsTest(t)

	// The owner of a moved instance can still read their recents on it: their
	// own rows are never migrated.
	alice.Moved = true
	src := resolveSource(alice, sourceRow(alice, "f"))
	assert.Equal(t, recents.SourceLive, src.State)
	assert.Same(t, alice, src.Instance)
}

func TestSourcesLive(t *testing.T) {
	alice, bob := setupRecentsTest(t)

	src := resolveSource(alice, sourceRow(bob, "f"))
	assert.Equal(t, recents.SourceLive, src.State)
	require.NotNil(t, src.Instance)
	assert.Equal(t, bob.DocID, src.Instance.DocID)
}

func TestSourcesGone(t *testing.T) {
	alice, _ := setupRecentsTest(t)

	src := resolveSource(alice, recents.FileDirActivity{
		DocID:         "unknown-instance-id:f",
		SourceLocator: "ghost.example.net/f",
	})
	assert.Equal(t, recents.SourceGone, src.State)
}

func TestSourcesMovedOnThisStack(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()

	t.Run("URL", func(t *testing.T) {
		markMoved(t, bob, "https://"+carol.Domain)
		src := resolveSource(alice, sourceRow(bob, "f"))
		assert.Equal(t, recents.SourceMoved, src.State)
		require.NotNil(t, src.Instance)
		assert.Equal(t, carol.DocID, src.Instance.DocID)
	})

	t.Run("Host", func(t *testing.T) {
		markMoved(t, bob, carol.Domain)
		src := resolveSource(alice, sourceRow(bob, "f"))
		assert.Equal(t, recents.SourceMoved, src.State)
		require.NotNil(t, src.Instance)
		assert.Equal(t, carol.DocID, src.Instance.DocID)
	})
}

func TestSourcesMovedToAnotherStack(t *testing.T) {
	alice, bob := setupRecentsTest(t)

	markMoved(t, bob, "https://elsewhere.example.net")
	src := resolveSource(alice, sourceRow(bob, "f"))
	assert.Equal(t, recents.SourceGone, src.State)
}

func TestSourcesMovedToAnotherStackWithTheSameDomain(t *testing.T) {
	alice, bob := setupRecentsTest(t)

	// No other instance of this stack can have the domain of the source: the
	// target is on another stack.
	markMoved(t, bob, "https://"+bob.Domain)
	src := resolveSource(alice, sourceRow(bob, "f"))
	assert.Equal(t, recents.SourceGone, src.State)
}

func TestSourcesMovedToAnUnknownTarget(t *testing.T) {
	alice, bob := setupRecentsTest(t)

	markMoved(t, bob, "")
	src := resolveSource(alice, sourceRow(bob, "f"))
	assert.Equal(t, recents.SourceUnavailable, src.State, "the rows may still have a successor")
}

func TestSourcesDeletedWithASuccessor(t *testing.T) {
	alice, _ := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()

	oldDomain := strings.ToLower(t.Name()) + ".old.example.net"
	updateStoredInstance(t, carol, func(stored *instance.Instance) { stored.OldDomain = oldDomain })
	src := resolveSource(alice, recents.FileDirActivity{
		DocID:         "unknown-instance-id:f",
		SourceLocator: oldDomain + "/f",
	})
	assert.Equal(t, recents.SourceMoved, src.State)
	require.NotNil(t, src.Instance)
	assert.Equal(t, carol.DocID, src.Instance.DocID)
	assert.NotPanics(t, func() { src.Instance.VFS() })
}

func TestSourcesTransientFailure(t *testing.T) {
	alice, bob := setupRecentsTest(t)

	original := *recents.GetInstancesByIDs
	*recents.GetInstancesByIDs = func([]string) (map[string]*instance.Instance, error) {
		return nil, errors.New("CouchDB is down")
	}
	t.Cleanup(func() { *recents.GetInstancesByIDs = original })

	src := resolveSource(alice, sourceRow(bob, "f"))
	assert.Equal(t, recents.SourceUnavailable, src.State, "a failure never makes a source gone")
}

// createRow writes a recents row with the dates of its activities.
func createRow(t *testing.T, inst, src *instance.Instance, fileID, edited, viewed string) {
	t.Helper()
	row := sourceRow(src, fileID)
	row.LastEditedAt = edited
	row.LastViewedAt = viewed
	row.LastActivityAt = max(edited, viewed)
	row.SchemaVersion = recents.SchemaVersion
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, &row))
}

func TestRepairsMigrateMergesTheDates(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()

	createRow(t, alice, bob, "f", "2026-01-02T00:00:00Z", "")
	createRow(t, alice, carol, "f", "", "2026-01-03T00:00:00Z")
	createRow(t, alice, bob, "g", "2026-01-01T00:00:00Z", "")
	keys := []string{
		recents.ResourceKey(bob, "f"),
		recents.ResourceKey(carol, "f"),
		recents.ResourceKey(bob, "g"),
	}
	found, err := recents.GetRows(alice, keys)
	require.NoError(t, err)
	require.Len(t, found, 3)

	var repairs recents.Repairs
	repairs.Migrate(found[keys[0]], found[keys[1]], carol, "f")
	repairs.Migrate(found[keys[2]], nil, carol, "g")
	repairs.Apply(alice)

	all := rows(t, alice)
	require.Len(t, all, 2, "the old rows are deleted")
	f := getRow(t, alice, recents.ResourceKey(carol, "f"))
	assert.Equal(t, "2026-01-02T00:00:00Z", f.LastEditedAt)
	assert.Equal(t, "2026-01-03T00:00:00Z", f.LastViewedAt)
	assert.Equal(t, "2026-01-03T00:00:00Z", f.LastActivityAt)
	g := getRow(t, alice, recents.ResourceKey(carol, "g"))
	assert.Equal(t, carol.Domain+"/g", g.SourceLocator)
	assert.Equal(t, "2026-01-01T00:00:00Z", g.LastEditedAt)
	assert.Equal(t, "2026-01-01T00:00:00Z", g.LastActivityAt)
}

func TestRepairsMigrateIntoARefreshedRow(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()

	createRow(t, alice, bob, "f", "2026-01-02T00:00:00Z", "")
	createRow(t, alice, carol, "f", "", "2026-01-03T00:00:00Z")
	keys := []string{recents.ResourceKey(bob, "f"), recents.ResourceKey(carol, "f")}
	found, err := recents.GetRows(alice, keys)
	require.NoError(t, err)

	// The row of the successor is refreshed and is the target of a merged
	// migration in the same traversal: both are one write.
	var repairs recents.Repairs
	repairs.Refresh(found[keys[1]], carol.Domain+"/f")
	repairs.Migrate(found[keys[0]], found[keys[1]], carol, "f")
	repairs.Apply(alice)

	all := rows(t, alice)
	require.Len(t, all, 1, "the old row is deleted")
	f := getRow(t, alice, keys[1])
	assert.Equal(t, carol.Domain+"/f", f.SourceLocator)
	assert.Equal(t, "2026-01-02T00:00:00Z", f.LastEditedAt)
	assert.Equal(t, "2026-01-03T00:00:00Z", f.LastViewedAt)
}

func TestRepairsKeepTheOldRowWhenTheNewOneFails(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()

	createRow(t, alice, bob, "f", "2026-01-02T00:00:00Z", "")
	createRow(t, alice, carol, "f", "", "2026-01-03T00:00:00Z")
	old := getRow(t, alice, recents.ResourceKey(bob, "f"))

	// The row of the successor is written meanwhile: the creation of the new
	// row is a conflict, and the old row must not be deleted.
	var repairs recents.Repairs
	repairs.Migrate(&old, nil, carol, "f")
	repairs.Apply(alice)

	assert.Len(t, rows(t, alice), 2)
	kept := getRow(t, alice, recents.ResourceKey(bob, "f"))
	assert.Equal(t, old.DocRev, kept.DocRev)
}
