package recents_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRow stores a row with the given dates of activity.
func newRow(t *testing.T, inst *instance.Instance, id, edited, viewed string) *recents.FileDirActivity {
	t.Helper()
	row := &recents.FileDirActivity{
		DocID: id, LastEditedAt: edited, LastViewedAt: viewed, LastActivityAt: max(edited, viewed),
		Name: "old.txt", Type: consts.FileType, UpdatedAt: "2026-01-01T00:00:00Z",
		DirID: "old-dir", Path: "/old.txt", SchemaVersion: recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, row))
	return row
}

func assertNoRow(t *testing.T, inst *instance.Instance, id string) {
	t.Helper()
	err := couchdb.GetDoc(inst, consts.FilesRecents, id, &recents.FileDirActivity{})
	assert.True(t, couchdb.IsNotFoundError(err), "unexpected error: %v", err)
}

func TestSyncRows(t *testing.T) {
	inst, _ := setupRecordTest(t)

	const edited, viewed, later = "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z", "2026-03-03T00:00:00Z"
	meta := recents.Metadata{
		Name: "new.txt", Type: consts.FileType, Mime: "text/plain", Class: "text",
		Size: "12", UpdatedAt: "2026-02-02T00:00:00Z", DirID: "new-dir", Path: "/new/new.txt",
		DriveID: "new-drive", OwnerDomain: "owner.example",
	}

	t.Run("RowsByIDsKeepsTheExistingRows", func(t *testing.T) {
		a, b := newRow(t, inst, "rows-a", edited, viewed), newRow(t, inst, "rows-b", edited, viewed)
		deleted := newRow(t, inst, "rows-deleted", edited, viewed)
		require.NoError(t, couchdb.DeleteDoc(inst, deleted))

		rows, err := recents.RowsByIDs(inst, []string{a.DocID, "rows-none", deleted.DocID, b.DocID})
		require.NoError(t, err)
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.DocID)
		}
		assert.ElementsMatch(t, []string{a.DocID, b.DocID}, ids)
	})

	t.Run("RowsByIDsWithoutDatabase", func(t *testing.T) {
		noDB := &instance.Instance{Domain: "no-recents-db.example", Prefix: "no-recents-db"}
		rows, err := recents.RowsByIDs(noDB, []string{"any"})
		require.NoError(t, err)
		assert.Empty(t, rows)
	})

	t.Run("RefreshWritesTheMetadataAndKeepsTheDates", func(t *testing.T) {
		require.NoError(t, recents.RefreshRow(inst, newRow(t, inst, "refresh", edited, viewed), meta))

		got := getRow(t, inst, "refresh")
		assert.Equal(t, recents.FileDirActivity{
			DocID: "refresh", DocRev: got.DocRev, DriveID: "new-drive", OwnerDomain: "owner.example",
			LastEditedAt: edited, LastViewedAt: viewed, LastActivityAt: viewed,
			Name: "new.txt", Type: consts.FileType, Mime: "text/plain", Class: "text", Size: "12",
			UpdatedAt: "2026-02-02T00:00:00Z", DirID: "new-dir", Path: "/new/new.txt",
			SchemaVersion: recents.SchemaVersion,
		}, got)

		// Writing the same metadata again changes nothing.
		require.NoError(t, recents.RefreshRow(inst, &got, meta))
		assert.Equal(t, got.DocRev, getRow(t, inst, "refresh").DocRev)
	})

	t.Run("RefreshAfterAConcurrentActivityKeepsItsDate", func(t *testing.T) {
		stale := newRow(t, inst, "conflict", edited, viewed)
		fresh := getRow(t, inst, "conflict")
		fresh.LastEditedAt, fresh.LastActivityAt = later, later
		require.NoError(t, couchdb.UpdateDoc(inst, &fresh))

		require.NoError(t, recents.RefreshRow(inst, stale, meta))

		got := getRow(t, inst, "conflict")
		assert.Equal(t, "new.txt", got.Name)
		assert.Equal(t, later, got.LastEditedAt)
		assert.Equal(t, later, got.LastActivityAt)
	})

	t.Run("RefreshOfADeletedRowDoesNotCreateIt", func(t *testing.T) {
		stale := newRow(t, inst, "gone", edited, viewed)
		fresh := getRow(t, inst, "gone")
		require.NoError(t, couchdb.DeleteDoc(inst, &fresh))

		require.NoError(t, recents.RefreshRow(inst, stale, meta))
		assertNoRow(t, inst, "gone")
	})

	t.Run("DeleteRowWhateverWasWrittenSince", func(t *testing.T) {
		stale := newRow(t, inst, "delete", edited, viewed)
		fresh := getRow(t, inst, "delete")
		fresh.LastViewedAt = later
		require.NoError(t, couchdb.UpdateDoc(inst, &fresh))

		require.NoError(t, recents.DeleteRow(inst, stale))
		assertNoRow(t, inst, "delete")

		// A row already gone is not an error.
		require.NoError(t, recents.DeleteRow(inst, stale))
	})
}
