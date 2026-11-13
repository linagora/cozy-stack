package recents_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRowsByOwnerDomain(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()

	create := func(t *testing.T, id, ownerDomain string) {
		t.Helper()
		require.NoError(t, couchdb.CreateNamedDocWithDB(inst, &recents.FileDirActivity{
			DocID:          id,
			OwnerDomain:    ownerDomain,
			LastActivityAt: "2026-01-01T00:00:00Z",
			Name:           id,
			Type:           consts.FileType,
			SchemaVersion:  recents.SchemaVersion,
		}))
	}

	// Two rows on the items of one owner, one on another owner, and one of an
	// item of the user, whose owner_domain is empty.
	create(t, "owner-a-1", "owner-a.example")
	create(t, "owner-a-2", "owner-a.example")
	create(t, "owner-b-1", "owner-b.example")
	create(t, "own", "")

	t.Run("KeepsTheRowsOfTheOwner", func(t *testing.T) {
		rows, err := recents.RowsByOwnerDomain(inst, "owner-a.example")
		require.NoError(t, err)
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.DocID)
		}
		assert.ElementsMatch(t, []string{"owner-a-1", "owner-a-2"}, ids)
	})

	t.Run("NoRowOfAnUnknownOwner", func(t *testing.T) {
		rows, err := recents.RowsByOwnerDomain(inst, "owner-c.example")
		require.NoError(t, err)
		assert.Empty(t, rows)
	})

	t.Run("WithoutDatabase", func(t *testing.T) {
		noDB := &instance.Instance{Domain: "no-recents-db.example", Prefix: "no-recents-db"}
		rows, err := recents.RowsByOwnerDomain(noDB, "owner-a.example")
		require.NoError(t, err)
		assert.Empty(t, rows)
	})
}
