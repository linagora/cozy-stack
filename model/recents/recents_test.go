package recents_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupRecentsTest(t *testing.T) (*instance.Instance, *instance.Instance) {
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

	alice := testutils.NewSetup(t, t.Name()+"alice").GetTestInstance()
	bob := testutils.NewSetup(t, t.Name()+"bob").GetTestInstance()
	return alice, bob
}

func rows(t *testing.T, inst *instance.Instance) []recents.FileDirActivity {
	t.Helper()
	var docs []recents.FileDirActivity
	require.NoError(t, couchdb.GetAllDocs(inst, consts.FilesRecents, &couchdb.AllDocsRequest{}, &docs))
	return docs
}

func getRow(t *testing.T, inst *instance.Instance, key string) recents.FileDirActivity {
	t.Helper()
	var doc recents.FileDirActivity
	require.NoError(t, couchdb.GetDoc(inst, consts.FilesRecents, key, &doc))
	return doc
}

// bumpRow moves the recorded dates of a row forward in time, to simulate a
// fresher activity already stored when an older one arrives late.
func bumpRow(t *testing.T, inst *instance.Instance, key string, at time.Time) {
	t.Helper()
	doc := getRow(t, inst, key)
	doc.LastEditedAt = at.UTC().Format(time.RFC3339)
	doc.LastViewedAt = doc.LastEditedAt
	doc.LastActivityAt = doc.LastEditedAt
	require.NoError(t, couchdb.UpdateDoc(inst, &doc))
}

func TestRecordActivityIsIdempotent(t *testing.T) {
	alice, _ := setupRecentsTest(t)
	key := recents.ResourceKey(alice, "file-1")

	require.NoError(t, recents.RecordActivity(alice, alice, "file-1", recents.KindEdit))
	first := getRow(t, alice, key)
	require.NotEmpty(t, first.LastEditedAt)
	assert.Equal(t, first.LastEditedAt, first.LastActivityAt)
	assert.Empty(t, first.LastViewedAt)
	assert.Equal(t, key, first.DocID)
	assert.Equal(t, alice.Domain+"/file-1", first.SourceLocator)
	assert.Equal(t, 1, first.SchemaVersion)

	// A late duplicate of the same activity must not move the dates.
	future := time.Now().Add(time.Hour)
	bumpRow(t, alice, key, future)
	require.NoError(t, recents.RecordActivity(alice, alice, "file-1", recents.KindEdit))

	assert.Len(t, rows(t, alice), 1)
	second := getRow(t, alice, key)
	assert.Equal(t, future.UTC().Format(time.RFC3339), second.LastEditedAt)
	assert.Equal(t, future.UTC().Format(time.RFC3339), second.LastActivityAt)
}

func TestRecordActivityKeepsComponentsDistinct(t *testing.T) {
	alice, _ := setupRecentsTest(t)
	key := recents.ResourceKey(alice, "file-2")

	require.NoError(t, recents.RecordActivity(alice, alice, "file-2", recents.KindEdit))
	edited := getRow(t, alice, key)

	require.NoError(t, recents.RecordActivity(alice, alice, "file-2", recents.KindView))
	doc := getRow(t, alice, key)

	assert.Equal(t, edited.LastEditedAt, doc.LastEditedAt)
	assert.NotEmpty(t, doc.LastViewedAt)
	assert.Equal(t, doc.LastActivityAt, max(doc.LastEditedAt, doc.LastViewedAt))
}

func TestRecordActivityIsMonotonic(t *testing.T) {
	alice, _ := setupRecentsTest(t)
	key := recents.ResourceKey(alice, "file-3")

	require.NoError(t, recents.RecordActivity(alice, alice, "file-3", recents.KindView))
	future := time.Now().Add(2 * time.Hour)
	bumpRow(t, alice, key, future)

	// An edit older than the stored activity must not downgrade the row.
	require.NoError(t, recents.RecordActivity(alice, alice, "file-3", recents.KindEdit))
	doc := getRow(t, alice, key)
	assert.Equal(t, future.UTC().Format(time.RFC3339), doc.LastViewedAt)
	assert.Equal(t, future.UTC().Format(time.RFC3339), doc.LastActivityAt)
}

func TestRecordActivityPerUser(t *testing.T) {
	alice, bob := setupRecentsTest(t)

	require.NoError(t, recents.RecordActivity(alice, alice, "shared-file", recents.KindEdit))
	require.NoError(t, recents.RecordActivity(bob, alice, "shared-file", recents.KindEdit))

	aliceRow := getRow(t, alice, recents.ResourceKey(alice, "shared-file"))
	bobRow := getRow(t, bob, recents.ResourceKey(alice, "shared-file"))
	assert.NotEmpty(t, aliceRow.LastEditedAt)
	assert.NotEmpty(t, bobRow.LastEditedAt)
	assert.Len(t, rows(t, alice), 1)
	assert.Len(t, rows(t, bob), 1)
}

func TestResourceKeyUsesSourceInstanceDocID(t *testing.T) {
	alice, bob := setupRecentsTest(t)

	// The same file ID on two instances must not collide.
	require.NoError(t, recents.RecordActivity(alice, alice, "same-id", recents.KindEdit))
	require.NoError(t, recents.RecordActivity(alice, bob, "same-id", recents.KindView))

	docs := rows(t, alice)
	require.Len(t, docs, 2)
	keys := []string{docs[0].DocID, docs[1].DocID}
	assert.Contains(t, keys, alice.DocID+":same-id")
	assert.Contains(t, keys, bob.DocID+":same-id")
	assert.NotEqual(t, recents.ResourceKey(alice, "same-id"), recents.ResourceKey(bob, "same-id"))
}

func TestRecordActivityMinRewriteInterval(t *testing.T) {
	alice, _ := setupRecentsTest(t)
	key := recents.ResourceKey(alice, "file-4")
	start := time.Now().Add(-time.Hour)

	require.NoError(t, recents.RecordActivityAt(alice, alice, "file-4", recents.KindEdit, start))
	first := getRow(t, alice, key)

	// 30 s later: too close, the row is not rewritten.
	require.NoError(t, recents.RecordActivityAt(alice, alice, "file-4", recents.KindEdit, start.Add(30*time.Second)))
	assert.Equal(t, first.DocRev, getRow(t, alice, key).DocRev)

	// 90 s later: a new revision.
	require.NoError(t, recents.RecordActivityAt(alice, alice, "file-4", recents.KindEdit, start.Add(90*time.Second)))
	doc := getRow(t, alice, key)
	assert.NotEqual(t, first.DocRev, doc.DocRev)
	assert.Equal(t, start.Add(90*time.Second).UTC().Format(time.RFC3339), doc.LastEditedAt)
}

func TestRecordActivityRefreshesSourceLocator(t *testing.T) {
	alice, _ := setupRecentsTest(t)
	key := recents.ResourceKey(alice, "file-5")

	require.NoError(t, recents.RecordActivity(alice, alice, "file-5", recents.KindEdit))
	doc := getRow(t, alice, key)
	doc.SourceLocator = "old-domain.example/file-5"
	require.NoError(t, couchdb.UpdateDoc(alice, &doc))

	// Even a replayed activity, which moves no date, fixes the locator.
	require.NoError(t, recents.RecordActivity(alice, alice, "file-5", recents.KindEdit))
	assert.Equal(t, alice.Domain+"/file-5", getRow(t, alice, key).SourceLocator)
}

func TestRecordActivityOnFolder(t *testing.T) {
	alice, _ := setupRecentsTest(t)

	require.NoError(t, recents.RecordActivity(alice, alice, "dir-1", recents.KindEdit))
	doc := getRow(t, alice, recents.ResourceKey(alice, "dir-1"))
	assert.NotEmpty(t, doc.LastEditedAt)
	assert.Empty(t, doc.LastViewedAt)
}

func TestPublicIDHidesTheResourceKey(t *testing.T) {
	key := "9b2e4c0a7f1d4e8b9c3a5d6e7f801234:file-1"

	id := recents.PublicID(key)
	assert.Len(t, id, 32)
	assert.Equal(t, id, recents.PublicID(key), "the public ID of a row is stable")
	assert.NotEqual(t, id, recents.PublicID(key+"0"))
	assert.NotContains(t, id, "9b2e4c0a7f1d4e8b9c3a5d6e7f801234")
}
