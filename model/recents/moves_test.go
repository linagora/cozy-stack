package recents_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoveRow(t *testing.T) {
	inst, _ := setupRecordTest(t)
	const jan1, jan2, mar1, may1 = "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z", "2026-03-01T00:00:00Z", "2026-05-01T00:00:00Z"
	meta := recents.Metadata{
		Name: "new", Type: consts.FileType, Path: "/sub/new",
		DriveID: "drive-1", OwnerDomain: "owner.example.net",
	}

	t.Run("WritesTheNewRowWithTheMetadataAndDeletesTheOld", func(t *testing.T) {
		old := newRow(t, inst, "move-old", jan1, jan2)
		require.NoError(t, recents.MoveRow(inst, old, "move-new", meta))

		assertNoRow(t, inst, "move-old")
		got := getRow(t, inst, "move-new")
		assert.Equal(t, recents.FileDirActivity{
			DocID: "move-new", DocRev: got.DocRev, DriveID: "drive-1", OwnerDomain: "owner.example.net",
			LastEditedAt: jan1, LastViewedAt: jan2, LastActivityAt: jan2,
			Name: "new", Type: consts.FileType, Path: "/sub/new", SchemaVersion: recents.SchemaVersion,
		}, got)
	})

	t.Run("MergesTheDatesToTheMaxWithAnExistingRow", func(t *testing.T) {
		// The row that exists has the later edit, and the moved one the later view.
		newRow(t, inst, "merge-new", mar1, jan1)
		old := newRow(t, inst, "merge-old", jan1, may1)
		require.NoError(t, recents.MoveRow(inst, old, "merge-new", meta))

		assertNoRow(t, inst, "merge-old")
		got := getRow(t, inst, "merge-new")
		assert.Equal(t, mar1, got.LastEditedAt)
		assert.Equal(t, may1, got.LastViewedAt)
		assert.Equal(t, may1, got.LastActivityAt)
		assert.Equal(t, "new", got.Name)
	})
}
