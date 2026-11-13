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
	const owner = "owner.example.net"

	t.Run("WritesTheRowUnderTheNewIDAndDeletesTheOld", func(t *testing.T) {
		old := newRow(t, inst, "move-old", jan1, jan2)
		old.Name, old.Mime, old.Class = "moved.txt", "text/plain", "text"
		require.NoError(t, recents.MoveRow(inst, old, "move-new", owner))

		assertNoRow(t, inst, "move-old")
		got := getRow(t, inst, "move-new")
		want := recents.FileDirActivity{
			DocID: "move-new", DocRev: got.DocRev, OwnerDomain: owner,
			LastEditedAt: jan1, LastViewedAt: jan2, LastActivityAt: jan2,
			Name: "moved.txt", Type: consts.FileType, Mime: "text/plain", Class: "text",
			SchemaVersion: recents.SchemaVersion,
		}
		assert.Equal(t, want, got)

		// A replay finds the old row gone and changes nothing.
		require.NoError(t, recents.MoveRow(inst, old, "move-new", owner))
		assertNoRow(t, inst, "move-old")
		replayed := getRow(t, inst, "move-new")
		want.DocRev = replayed.DocRev
		assert.Equal(t, want, replayed)
	})

	t.Run("MergesTheDatesToTheMaxWithAnExistingRow", func(t *testing.T) {
		// The row that exists has the later edit, and the moved one the later
		// view: it is the last one that the user has seen.
		newRow(t, inst, "merge-new", mar1, jan1)
		old := newRow(t, inst, "merge-old", jan1, may1)
		old.Name, old.OwnerDomain = "moved.txt", "source.example.net"
		// The user is the owner of the destination.
		require.NoError(t, recents.MoveRow(inst, old, "merge-new", ""))

		assertNoRow(t, inst, "merge-old")
		got := getRow(t, inst, "merge-new")
		assert.Equal(t, mar1, got.LastEditedAt)
		assert.Equal(t, may1, got.LastViewedAt)
		assert.Equal(t, may1, got.LastActivityAt)
		assert.Equal(t, "moved.txt", got.Name)
		assert.Empty(t, got.OwnerDomain)
	})

	t.Run("KeepsTheNameOfAMoreRecentRow", func(t *testing.T) {
		newRow(t, inst, "recent-new", may1, jan1)
		old := newRow(t, inst, "recent-old", jan1, mar1)
		old.Name = "moved.txt"
		require.NoError(t, recents.MoveRow(inst, old, "recent-new", owner))

		assertNoRow(t, inst, "recent-old")
		got := getRow(t, inst, "recent-new")
		assert.Equal(t, may1, got.LastEditedAt)
		assert.Equal(t, mar1, got.LastViewedAt)
		assert.Equal(t, "old.txt", got.Name)
		assert.Equal(t, owner, got.OwnerDomain)
	})
}
