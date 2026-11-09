package recents_test

import (
	"encoding/base64"
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

func readTestInstance(t *testing.T) *instance.Instance {
	t.Helper()
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	return testutils.NewSetup(t, t.Name()).GetTestInstance()
}

func createReadRow(t *testing.T, inst *instance.Instance, id, date string) {
	t.Helper()
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, &recents.FileDirActivity{
		DocID:          id,
		LastActivityAt: date,
		Name:           id,
		Type:           consts.FileType,
		SchemaVersion:  recents.SchemaVersion,
	}))
}

func candidateIDs(rows []recents.FileDirActivity) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].DocID
	}
	return ids
}

func TestCursorIsTheBase64OfThePosition(t *testing.T) {
	pos := recents.Position{LastActivityAt: "2026-11-05T10:00:00Z", DocID: "019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34"}

	cursor, err := pos.Cursor()
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"last_activity_at": "2026-11-05T10:00:00Z",
		"id": "019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34"
	}`, string(raw))

	parsed, err := recents.ParseCursor(cursor)
	require.NoError(t, err)
	assert.Equal(t, pos, parsed)
}

func TestParseCursorRejectsAnInvalidCursor(t *testing.T) {
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

	for name, cursor := range map[string]string{
		"empty":          "",
		"not base64":     "not base64!",
		"not JSON":       encode("not json"),
		"null":           encode("null"),
		"not an object":  encode(`["2026-11-05T10:00:00Z", "file-id"]`),
		"wrong type":     encode(`{"last_activity_at": 1, "id": "file-id"}`),
		"no date":        encode(`{"id": "file-id"}`),
		"no ID":          encode(`{"last_activity_at": "2026-11-05T10:00:00Z"}`),
		"empty date":     encode(`{"last_activity_at": "", "id": "file-id"}`),
		"empty ID":       encode(`{"last_activity_at": "2026-11-05T10:00:00Z", "id": ""}`),
		"not a date":     encode(`{"last_activity_at": "yesterday", "id": "file-id"}`),
		"trailing bytes": encode(`{"last_activity_at": "2026-11-05T10:00:00Z", "id": "file-id"} x`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := recents.ParseCursor(cursor)
			assert.Error(t, err)
		})
	}
}

func TestCandidates(t *testing.T) {
	inst := readTestInstance(t)

	createReadRow(t, inst, "id-b", "2026-11-03T10:00:00Z")
	createReadRow(t, inst, "id-e", "2026-11-01T10:00:00Z")
	createReadRow(t, inst, "id-a", "2026-11-03T10:00:00Z")
	createReadRow(t, inst, "id-d", "2026-11-02T10:00:00Z")
	createReadRow(t, inst, "id-c", "2026-11-03T10:00:00Z")

	t.Run("sorted by date then ID, both descending", func(t *testing.T) {
		rows, err := recents.Candidates(inst, 10, recents.Position{})
		require.NoError(t, err)
		assert.Equal(t, []string{"id-c", "id-b", "id-a", "id-d", "id-e"}, candidateIDs(rows))
	})

	t.Run("the limit bounds the rows", func(t *testing.T) {
		rows, err := recents.Candidates(inst, 2, recents.Position{})
		require.NoError(t, err)
		assert.Equal(t, []string{"id-c", "id-b"}, candidateIDs(rows))
	})

	t.Run("resumes strictly after a position among same-date rows", func(t *testing.T) {
		rows, err := recents.Candidates(inst, 10, recents.Position{
			LastActivityAt: "2026-11-03T10:00:00Z",
			DocID:          "id-b",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"id-a", "id-d", "id-e"}, candidateIDs(rows))
	})

	t.Run("resumes strictly after the last same-date row", func(t *testing.T) {
		rows, err := recents.Candidates(inst, 10, recents.Position{
			LastActivityAt: "2026-11-03T10:00:00Z",
			DocID:          "id-a",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"id-d", "id-e"}, candidateIDs(rows))
	})

	t.Run("a position needs no row of its own", func(t *testing.T) {
		rows, err := recents.Candidates(inst, 10, recents.Position{
			LastActivityAt: "2026-11-02T12:00:00Z",
			DocID:          "gone",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"id-d", "id-e"}, candidateIDs(rows))
	})

	t.Run("nothing follows the last row", func(t *testing.T) {
		rows, err := recents.Candidates(inst, 10, recents.Position{
			LastActivityAt: "2026-11-01T10:00:00Z",
			DocID:          "id-e",
		})
		require.NoError(t, err)
		assert.Empty(t, rows)
	})
}

func TestCandidatesReadsTheStoredFields(t *testing.T) {
	inst := readTestInstance(t)

	stored := &recents.FileDirActivity{
		DocID:          "019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34",
		DriveID:        "019a6f2e-0000-7000-8000-000000000001",
		OwnerDomain:    "alice.example.net",
		LastEditedAt:   "2026-11-05T09:00:00Z",
		LastViewedAt:   "2026-11-05T10:00:00Z",
		LastActivityAt: "2026-11-05T10:00:00Z",
		Name:           "report.docx",
		Type:           consts.FileType,
		Mime:           "text/plain",
		Class:          "text",
		Size:           "1234",
		UpdatedAt:      "2026-11-05T09:00:00Z",
		DirID:          "019a6f2e-0000-7000-8000-000000000002",
		Path:           "/Reports/report.docx",
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, stored))

	rows, err := recents.Candidates(inst, 1, recents.Position{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, *stored, rows[0])
}

func TestCandidatesOfAMissingDatabase(t *testing.T) {
	inst := readTestInstance(t)
	require.NoError(t, couchdb.DeleteDB(inst, consts.FilesRecents))

	rows, err := recents.Candidates(inst, 10, recents.Position{})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestCandidatesOfAnEmptyDatabase(t *testing.T) {
	inst := readTestInstance(t)

	rows, err := recents.Candidates(inst, 10, recents.Position{})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestPositionOf(t *testing.T) {
	row := &recents.FileDirActivity{DocID: "id-a", LastActivityAt: "2026-11-03T10:00:00Z"}
	assert.Equal(t, recents.Position{LastActivityAt: "2026-11-03T10:00:00Z", DocID: "id-a"}, recents.PositionOf(row))
}
