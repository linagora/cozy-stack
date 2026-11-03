package recents_test

import (
	"fmt"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDestroyTarget(t *testing.T) {
	_, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()
	dave := testutils.NewSetup(t, t.Name()+"dave").GetTestInstance()
	destroyTarget := func(inst *instance.Instance) string {
		t.Helper()
		target, err := recents.DestroyTarget(inst)
		require.NoError(t, err)
		return docID(target)
	}

	assert.Empty(t, destroyTarget(bob), "the rows on a destroyed instance are deleted")

	markMoved(t, bob, "https://"+carol.Domain)
	bob.Moved = true
	assert.Equal(t, carol.DocID, destroyTarget(bob),
		"the rows on a moved instance are migrated to its successor")

	markMoved(t, bob, "https://elsewhere.example.net")
	assert.Empty(t, destroyTarget(bob), "a successor on another stack is not followed")

	// dave has not moved: an instance that declares its domain as old domain
	// may have kept it since the domain was given back to dave.
	updateStoredInstance(t, carol, func(stored *instance.Instance) { stored.OldDomain = dave.Domain })
	assert.Empty(t, destroyTarget(dave), "the old domain of another instance is not followed")
}

// docID returns the ID of an instance, or an empty string for nil.
func docID(inst *instance.Instance) string {
	if inst == nil {
		return ""
	}
	return inst.DocID
}

func TestFollowTarget(t *testing.T) {
	_, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()
	dave := testutils.NewSetup(t, t.Name()+"dave").GetTestInstance()
	followTarget := func(inst *instance.Instance) string {
		t.Helper()
		target, err := recents.FollowTarget(inst)
		require.NoError(t, err)
		return docID(target)
	}

	markMoved(t, bob, "https://"+carol.Domain)
	assert.Equal(t, carol.DocID, followTarget(bob), "the rows are migrated to the target on this stack")

	markMoved(t, carol, "https://"+dave.Domain)
	assert.Equal(t, dave.DocID, followTarget(bob), "the chain of moves is followed")

	markMoved(t, bob, "https://elsewhere.example.net")
	assert.Empty(t, followTarget(bob), "the rows are deleted when the target is on another stack")

	// A move to another stack that keeps the domain: the target found on
	// this stack is the source itself.
	markMoved(t, bob, "https://"+bob.Domain)
	assert.Empty(t, followTarget(bob), "the rows are deleted when the target is the source itself")

	markMoved(t, dave, "")
	_, err := recents.FollowTarget(dave)
	assert.ErrorIs(t, err, recents.ErrUnknownMove, "the rows are left when the target is unknown")
}

func TestEachSourceBatchGivesEachRowOnce(t *testing.T) {
	alice, bob := setupRecentsTest(t)

	n := 2*recents.SourceBatchSize + 1
	docs := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		row := sourceRow(bob, fmt.Sprintf("f%d", i))
		row.LastEditedAt = "2026-01-01T00:00:00Z"
		row.LastActivityAt = row.LastEditedAt
		row.SchemaVersion = recents.SchemaVersion
		docs = append(docs, &row)
	}
	_, err := couchdb.BulkWriteDocs(alice, consts.FilesRecents, docs)
	require.NoError(t, err)

	// No row is changed, as if every change had failed: each next batch
	// starts with the last row of the previous one, which must not be given
	// again.
	given := make(map[string]int)
	require.NoError(t, recents.EachSourceBatch(alice, bob.DocID, func(batch []*recents.FileDirActivity) error {
		for _, row := range batch {
			given[row.DocID]++
		}
		return nil
	}))
	assert.Len(t, given, n)
	for id, count := range given {
		assert.Equal(t, 1, count, id)
	}
}

// createNeighbourRows writes rows whose keys start with the ID of src, but for
// other instance IDs, sorted just before and just after the range of the rows
// of src.
func createNeighbourRows(t *testing.T, inst, src *instance.Instance) []string {
	t.Helper()
	var keys []string
	for _, suffix := range []string{"0", "z"} {
		row := recents.FileDirActivity{
			DocID:          src.DocID + suffix + ":f",
			SourceLocator:  "neighbour.example.net/f",
			LastEditedAt:   "2026-01-04T00:00:00Z",
			LastActivityAt: "2026-01-04T00:00:00Z",
			SchemaVersion:  recents.SchemaVersion,
		}
		require.NoError(t, couchdb.CreateNamedDocWithDB(inst, &row))
		keys = append(keys, row.DocID)
	}
	return keys
}

// revs returns the revision of each row of an instance, by key.
func revs(t *testing.T, inst *instance.Instance) map[string]string {
	t.Helper()
	revs := make(map[string]string)
	for _, row := range rows(t, inst) {
		revs[row.DocID] = row.DocRev
	}
	return revs
}

// setupSourceRows writes the rows of alice on the items of bob and carol,
// with a row of her own and the rows of neighbour keys of bob. It returns the
// keys of the rows that are not on the items of bob nor carol.
func setupSourceRows(t *testing.T, alice, bob, carol *instance.Instance) []string {
	t.Helper()
	createRow(t, alice, bob, "f", "2026-01-02T00:00:00Z", "")
	createRow(t, alice, bob, "g", "2026-01-01T00:00:00Z", "")
	createRow(t, alice, carol, "f", "", "2026-01-03T00:00:00Z")
	createRow(t, alice, alice, "own", "2026-01-05T00:00:00Z", "")
	return append(createNeighbourRows(t, alice, bob), recents.ResourceKey(alice, "own"))
}

func TestMigrateSource(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()
	others := setupSourceRows(t, alice, bob, carol)
	kept := revs(t, alice)

	require.NoError(t, recents.MigrateSource(alice, bob.DocID, carol))

	after := revs(t, alice)
	assert.Len(t, after, 5, "the rows of bob are moved to carol, the others are kept")
	assert.NotContains(t, after, recents.ResourceKey(bob, "f"))
	assert.NotContains(t, after, recents.ResourceKey(bob, "g"))
	for _, key := range others {
		assert.Equal(t, kept[key], after[key], "the own rows and the neighbour keys are not changed")
	}

	f := getRow(t, alice, recents.ResourceKey(carol, "f"))
	assert.Equal(t, "2026-01-02T00:00:00Z", f.LastEditedAt)
	assert.Equal(t, "2026-01-03T00:00:00Z", f.LastViewedAt)
	assert.Equal(t, "2026-01-03T00:00:00Z", f.LastActivityAt)
	g := getRow(t, alice, recents.ResourceKey(carol, "g"))
	assert.Equal(t, carol.Domain+"/g", g.SourceLocator)
	assert.Equal(t, "2026-01-01T00:00:00Z", g.LastEditedAt)
	assert.Equal(t, "2026-01-01T00:00:00Z", g.LastActivityAt)

	require.NoError(t, recents.MigrateSource(alice, bob.DocID, carol))
	assert.Equal(t, after, revs(t, alice), "a second migration is a no-op")
}

func TestMigrateSourceByBatches(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()

	n := 2*recents.SourceBatchSize + 1
	createRow(t, alice, bob, "f0", "2026-01-01T00:00:00Z", "")
	docs := make([]interface{}, 0, n-1)
	for i := 1; i < n; i++ {
		row := sourceRow(bob, fmt.Sprintf("f%d", i))
		row.LastEditedAt = "2026-01-01T00:00:00Z"
		row.LastActivityAt = row.LastEditedAt
		row.SchemaVersion = recents.SchemaVersion
		docs = append(docs, &row)
	}
	_, err := couchdb.BulkWriteDocs(alice, consts.FilesRecents, docs)
	require.NoError(t, err)

	require.NoError(t, recents.MigrateSource(alice, bob.DocID, carol))

	all := rows(t, alice)
	assert.Len(t, all, n)
	for _, row := range all {
		instID, _, ok := recents.ParseResourceKey(row.DocID)
		require.True(t, ok)
		assert.Equal(t, carol.DocID, instID)
	}
}

func TestDropSource(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()
	others := setupSourceRows(t, alice, bob, carol)
	kept := revs(t, alice)

	require.NoError(t, recents.DropSource(alice, bob.DocID))

	after := revs(t, alice)
	assert.Len(t, after, 4, "the rows of bob are deleted")
	for _, key := range append(others, recents.ResourceKey(carol, "f")) {
		assert.Equal(t, kept[key], after[key], "the other rows are not changed")
	}

	require.NoError(t, recents.DropSource(alice, bob.DocID))
	assert.Equal(t, after, revs(t, alice), "a second deletion is a no-op")

	// carol has no rows: nothing to delete.
	require.NoError(t, recents.DropSource(carol, bob.DocID))
}

func TestSourceOwnRowsAreKept(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()
	setupSourceRows(t, alice, bob, carol)
	kept := revs(t, alice)

	require.NoError(t, recents.MigrateSource(alice, alice.DocID, carol))
	require.NoError(t, recents.DropSource(alice, alice.DocID))
	assert.Equal(t, kept, revs(t, alice))
}

func TestSourceMembers(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()
	alias := "alias-" + alice.Domain
	alice.DomainAliases = append(alice.DomainAliases, alias)
	require.NoError(t, instance.Update(alice))
	testutils.WithFlag(t, alice, "drive.recents.enabled", true)

	members := func(urls ...string) []interface{} {
		list := make([]interface{}, len(urls))
		for i, u := range urls {
			list[i] = map[string]interface{}{"instance": u}
		}
		return list
	}
	for _, m := range [][]interface{}{
		members("https://"+bob.Domain, "https://"+alice.Domain, "https://elsewhere.example.net", ""),
		members("https://"+alias, "https://"+bob.Domain, "https://"+carol.Domain),
	} {
		doc := &couchdb.JSONDoc{Type: consts.Sharings, M: map[string]interface{}{"members": m}}
		require.NoError(t, couchdb.CreateDoc(bob, doc))
	}

	found := recents.SourceMembers(bob)
	require.Len(t, found, 1,
		"the source itself, the members on another stack and those without recents are skipped, "+
			"a member listed under an alias is given once")
	assert.Equal(t, alice.DocID, found[0].DocID)
	assert.Empty(t, recents.SourceMembers(alice), "no sharings")
}

func runSourceJob(t *testing.T, user *instance.Instance, msg recents.SourceMessage) {
	t.Helper()
	m, err := job.NewMessage(msg)
	require.NoError(t, err)
	j := &job.Job{JobID: t.Name(), Domain: user.Domain, WorkerType: recents.WorkerSource, Message: m}
	ctx, cancel := job.NewTaskContext(recents.WorkerSource, j, user)
	defer cancel()
	require.NoError(t, recents.WorkSource(ctx))
}

func TestWorkSource(t *testing.T) {
	alice, bob := setupRecentsTest(t)
	carol := testutils.NewSetup(t, t.Name()+"carol").GetTestInstance()
	dave := testutils.NewSetup(t, t.Name()+"dave").GetTestInstance()

	t.Run("Migrate", func(t *testing.T) {
		createRow(t, alice, bob, "m", "2026-01-01T00:00:00Z", "")
		runSourceJob(t, alice, recents.SourceMessage{OldDocID: bob.DocID, NewDocID: carol.DocID})
		assert.NotContains(t, revs(t, alice), recents.ResourceKey(bob, "m"))
		assert.Contains(t, revs(t, alice), recents.ResourceKey(carol, "m"))
	})

	t.Run("SuccessorDeleted", func(t *testing.T) {
		createRow(t, alice, dave, "n", "2026-01-01T00:00:00Z", "")
		runSourceJob(t, alice, recents.SourceMessage{OldDocID: dave.DocID, NewDocID: "deleted-instance-id"})
		assert.NotContains(t, revs(t, alice), recents.ResourceKey(dave, "n"))
	})

	t.Run("Drop", func(t *testing.T) {
		createRow(t, alice, carol, "d", "2026-01-01T00:00:00Z", "")
		runSourceJob(t, alice, recents.SourceMessage{OldDocID: carol.DocID})
		all := revs(t, alice)
		assert.NotContains(t, all, recents.ResourceKey(carol, "d"))
		assert.NotContains(t, all, recents.ResourceKey(carol, "m"))
	})
}
