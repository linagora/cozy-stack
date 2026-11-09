package recents_test

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestRowIsKeyedByTheItemID(t *testing.T) {
	fileID := "019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34"
	row := &recents.FileDirActivity{DocID: fileID}

	assert.Equal(t, fileID, row.ID())
	assert.Equal(t, consts.FilesRecents, row.DocType())

	raw, err := json.Marshal(row)
	require.NoError(t, err)
	var fields map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &fields))
	assert.Equal(t, fileID, fields["_id"])
}

func TestRowFormatOfADriveItem(t *testing.T) {
	row := &recents.FileDirActivity{
		DocID:          "019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34",
		DriveID:        "019a6f2e-0000-7000-8000-000000000001",
		OwnerDomain:    "alice.example.net",
		LastEditedAt:   "2026-11-05T09:00:00Z",
		LastViewedAt:   "2026-11-05T10:00:00Z",
		LastActivityAt: "2026-11-05T10:00:00Z",
		Name:           "report.docx",
		Type:           "file",
		Mime:           "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Class:          "text",
		Size:           "1234",
		UpdatedAt:      "2026-11-05T09:00:00Z",
		DirID:          "019a6f2e-0000-7000-8000-000000000002",
		Path:           "/Reports/report.docx",
		SchemaVersion:  recents.SchemaVersion,
	}

	raw, err := json.Marshal(row)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"_id": "019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34",
		"drive_id": "019a6f2e-0000-7000-8000-000000000001",
		"owner_domain": "alice.example.net",
		"last_edited_at": "2026-11-05T09:00:00Z",
		"last_viewed_at": "2026-11-05T10:00:00Z",
		"last_activity_at": "2026-11-05T10:00:00Z",
		"name": "report.docx",
		"type": "file",
		"mime": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"class": "text",
		"size": "1234",
		"updated_at": "2026-11-05T09:00:00Z",
		"dir_id": "019a6f2e-0000-7000-8000-000000000002",
		"path": "/Reports/report.docx",
		"schema_version": 1
	}`, string(raw))
}

func TestRowFormatOfAnItemOfTheInstance(t *testing.T) {
	row := &recents.FileDirActivity{
		DocID:          "019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34",
		LastEditedAt:   "2026-11-05T09:00:00Z",
		LastActivityAt: "2026-11-05T09:00:00Z",
		Name:           "Reports",
		Type:           "directory",
		UpdatedAt:      "2026-11-05T09:00:00Z",
		DirID:          consts.RootDirID,
		Path:           "/Reports",
		SchemaVersion:  recents.SchemaVersion,
	}

	raw, err := json.Marshal(row)
	require.NoError(t, err)
	var fields map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &fields))
	assert.NotContains(t, fields, "drive_id")
	assert.NotContains(t, fields, "owner_domain")
	assert.NotContains(t, fields, "mime")
	assert.NotContains(t, fields, "class")
	assert.NotContains(t, fields, "size")
	assert.Equal(t, "Reports", fields["name"])
}

func TestRowFormatOfAFileHasItsFileFields(t *testing.T) {
	row := &recents.FileDirActivity{
		DocID:          "019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34",
		LastEditedAt:   "2026-11-05T09:00:00Z",
		LastActivityAt: "2026-11-05T09:00:00Z",
		Name:           "notes.txt",
		Type:           "file",
		Mime:           "text/plain",
		Class:          "text",
		Size:           "12",
		UpdatedAt:      "2026-11-05T09:00:00Z",
		DirID:          consts.RootDirID,
		Path:           "/notes.txt",
		SchemaVersion:  recents.SchemaVersion,
	}

	raw, err := json.Marshal(row)
	require.NoError(t, err)
	var fields map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &fields))
	assert.Equal(t, "text/plain", fields["mime"])
	assert.Equal(t, "text", fields["class"])
	assert.Equal(t, "12", fields["size"])
}

func TestIsFixedID(t *testing.T) {
	for _, id := range []string{
		consts.RootDirID,
		consts.TrashDirID,
		consts.SharedWithMeDirID,
		consts.NoLongerSharedDirID,
		consts.SharedDrivesDirID,
	} {
		assert.True(t, recents.IsFixedID(id), id)
	}

	for _, id := range []string{
		"",
		"019a6f2e-7c1d-7e3a-9b42-5d8e1f0a2c34",
		"io.cozy.files",
		"io.cozy.filesystem",
		"my.io.cozy.files.root-dir",
	} {
		assert.False(t, recents.IsFixedID(id), id)
	}
}

func setupRecordTest(t *testing.T) (*instance.Instance, *instance.Instance) {
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
	testutils.WithFlag(t, alice, "drive.recents.enabled", true)
	testutils.WithFlag(t, bob, "drive.recents.enabled", true)
	return alice, bob
}

func createFile(t *testing.T, inst *instance.Instance, name string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, 5, nil,
		"text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = f.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return doc
}

func createDir(t *testing.T, inst *instance.Instance, name string) *vfs.DirDoc {
	t.Helper()
	doc, err := vfs.Mkdir(inst.VFS(), "/"+name, nil)
	require.NoError(t, err)
	return doc
}

func allRows(t *testing.T, inst *instance.Instance) []recents.FileDirActivity {
	t.Helper()
	var docs []recents.FileDirActivity
	err := couchdb.GetAllDocs(inst, consts.FilesRecents, &couchdb.AllDocsRequest{}, &docs)
	if couchdb.IsNoDatabaseError(err) {
		return nil
	}
	require.NoError(t, err)
	return docs
}

func getRow(t *testing.T, inst *instance.Instance, id string) recents.FileDirActivity {
	t.Helper()
	var doc recents.FileDirActivity
	require.NoError(t, couchdb.GetDoc(inst, consts.FilesRecents, id, &doc))
	return doc
}

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

func TestRecordActivityOnAFile(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "notes.txt")

	require.NoError(t, recents.RecordActivity(alice, alice, nil, doc, recents.KindEdit, nil))

	row := getRow(t, alice, doc.ID())
	assert.Equal(t, doc.ID(), row.DocID)
	assert.Equal(t, "notes.txt", row.Name)
	assert.Equal(t, consts.FileType, row.Type)
	assert.Equal(t, "text/plain", row.Mime)
	assert.Equal(t, "text", row.Class)
	assert.Equal(t, "5", row.Size)
	assert.Equal(t, doc.UpdatedAt.Format(time.RFC3339Nano), row.UpdatedAt)
	assert.Equal(t, consts.RootDirID, row.DirID)
	assert.Equal(t, "/notes.txt", row.Path)
	assert.Empty(t, row.DriveID)
	assert.Empty(t, row.OwnerDomain)
	assert.NotEmpty(t, row.LastEditedAt)
	assert.Empty(t, row.LastViewedAt)
	assert.Equal(t, row.LastEditedAt, row.LastActivityAt)
	assert.Equal(t, recents.SchemaVersion, row.SchemaVersion)
}

func TestRecordActivityOnAFolder(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createDir(t, alice, "Reports")

	require.NoError(t, recents.RecordActivity(alice, alice, doc, nil, recents.KindEdit, nil))

	row := getRow(t, alice, doc.ID())
	assert.Equal(t, "Reports", row.Name)
	assert.Equal(t, consts.DirType, row.Type)
	assert.Empty(t, row.Mime)
	assert.Empty(t, row.Class)
	assert.Empty(t, row.Size)
	assert.Equal(t, doc.UpdatedAt.Format(time.RFC3339Nano), row.UpdatedAt)
	assert.Equal(t, consts.RootDirID, row.DirID)
	assert.Equal(t, "/Reports", row.Path)
}

func TestRecordActivityOnAnItemOfADrive(t *testing.T) {
	alice, bob := setupRecordTest(t)
	doc := createFile(t, bob, "shared.txt")
	drive := &recents.Drive{ID: "the-sharing-id", Path: "/sub/shared.txt"}

	require.NoError(t, recents.RecordActivity(alice, bob, nil, doc, recents.KindEdit, drive))

	row := getRow(t, alice, doc.ID())
	assert.Equal(t, "the-sharing-id", row.DriveID)
	assert.Equal(t, bob.Domain, row.OwnerDomain)
	assert.Equal(t, "/sub/shared.txt", row.Path, "never the full path on the holder")
	assert.Equal(t, "shared.txt", row.Name)
	assert.Empty(t, allRows(t, bob), "the row lives on the instance of the actor only")
}

func TestRecordActivityOnAnotherInstanceNeedsADrive(t *testing.T) {
	alice, bob := setupRecordTest(t)
	doc := createFile(t, bob, "not-in-a-drive.txt")

	require.NoError(t, recents.RecordActivity(alice, bob, nil, doc, recents.KindEdit, nil))

	assert.Empty(t, allRows(t, alice))
}

func TestRecordActivityOnOwnItemHasNoDrive(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "own.txt")
	drive := &recents.Drive{ID: "the-sharing-id", Path: "/own.txt"}

	require.NoError(t, recents.RecordActivity(alice, alice, nil, doc, recents.KindEdit, drive))

	row := getRow(t, alice, doc.ID())
	assert.Empty(t, row.DriveID)
	assert.Empty(t, row.OwnerDomain)
	assert.Equal(t, "/own.txt", row.Path)
}

func TestRecordActivityIgnoresTheFixedIDs(t *testing.T) {
	alice, _ := setupRecordTest(t)
	root, err := alice.VFS().DirByID(consts.RootDirID)
	require.NoError(t, err)

	require.NoError(t, recents.RecordActivity(alice, alice, root, nil, recents.KindEdit, nil))

	assert.Empty(t, allRows(t, alice))
}

func TestRecordActivityChecksTheFlagOfTheActor(t *testing.T) {
	alice, bob := setupRecordTest(t)
	doc := createFile(t, bob, "flagged.txt")
	drive := &recents.Drive{ID: "the-sharing-id", Path: "/flagged.txt"}

	testutils.WithFlag(t, alice, "drive.recents.enabled", false)
	require.NoError(t, recents.RecordActivity(alice, bob, nil, doc, recents.KindEdit, drive))
	assert.Empty(t, allRows(t, alice), "the flag of the actor is off")

	testutils.WithFlag(t, alice, "drive.recents.enabled", true)
	testutils.WithFlag(t, bob, "drive.recents.enabled", false)
	require.NoError(t, recents.RecordActivity(alice, bob, nil, doc, recents.KindEdit, drive))
	assert.Len(t, allRows(t, alice), 1, "the flag of the holder does not count")
}

func TestRecordActivityKeepsOneDatePerKind(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "kinds.txt")

	require.NoError(t, recents.RecordActivity(alice, alice, nil, doc, recents.KindView, nil))
	viewed := getRow(t, alice, doc.ID())
	require.NotEmpty(t, viewed.LastViewedAt)
	require.Empty(t, viewed.LastEditedAt)

	// A view does not hide a later edit, in the memory of the stored activities
	// or in the row.
	require.NoError(t, recents.RecordActivity(alice, alice, nil, doc, recents.KindEdit, nil))
	row := getRow(t, alice, doc.ID())
	assert.NotEmpty(t, row.LastEditedAt)
	assert.Equal(t, viewed.LastViewedAt, row.LastViewedAt)
	assert.Equal(t, max(row.LastEditedAt, row.LastViewedAt), row.LastActivityAt)
	assert.Len(t, allRows(t, alice), 1)
}

func TestRecordActivityNeverMovesADateBack(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "monotonic.txt")
	now := time.Now()

	require.NoError(t, recents.RecordActivityAt(alice, alice, nil, doc, recents.KindEdit, nil, now))
	require.NoError(t, recents.RecordActivityAt(alice, alice, nil, doc, recents.KindEdit, nil, now.Add(-time.Hour)))
	require.NoError(t, recents.RecordActivityAt(alice, alice, nil, doc, recents.KindView, nil, now.Add(-time.Hour)))

	row := getRow(t, alice, doc.ID())
	assert.Equal(t, stamp(now), row.LastEditedAt)
	assert.Equal(t, stamp(now.Add(-time.Hour)), row.LastViewedAt)
	assert.Equal(t, stamp(now), row.LastActivityAt, "the sort key is the maximum of the dates")
}

func TestRecordActivityWritesDatesInASingleFormat(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "dates.txt")
	at := time.Date(2026, 11, 5, 10, 0, 0, 500_000_000, time.FixedZone("CET", 3600))

	require.NoError(t, recents.RecordActivityAt(alice, alice, nil, doc, recents.KindEdit, nil, at))
	require.NoError(t, recents.RecordActivityAt(alice, alice, nil, doc, recents.KindView, nil, at))

	row := getRow(t, alice, doc.ID())
	assert.Equal(t, "2026-11-05T09:00:00Z", row.LastEditedAt)
	assert.Equal(t, "2026-11-05T09:00:00Z", row.LastViewedAt)
	assert.Equal(t, "2026-11-05T09:00:00Z", row.LastActivityAt)
}

func TestRecordActivityMinRewriteInterval(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "interval.txt")
	start := time.Now().Add(-time.Hour)

	require.NoError(t, recents.RecordActivityAt(alice, alice, nil, doc, recents.KindEdit, nil, start))
	first := getRow(t, alice, doc.ID())

	// 30 s later: too close, the row is not rewritten.
	require.NoError(t, recents.RecordActivityAt(alice, alice, nil, doc, recents.KindEdit, nil, start.Add(30*time.Second)))
	assert.Equal(t, first.DocRev, getRow(t, alice, doc.ID()).DocRev)

	// 90 s later: a new date.
	require.NoError(t, recents.RecordActivityAt(alice, alice, nil, doc, recents.KindEdit, nil, start.Add(90*time.Second)))
	row := getRow(t, alice, doc.ID())
	assert.NotEqual(t, first.DocRev, row.DocRev)
	assert.Equal(t, stamp(start.Add(90*time.Second)), row.LastEditedAt)
}

func TestRecordActivityWithinTheIntervalWritesChangedMetadata(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "before.txt")

	require.NoError(t, recents.RecordActivity(alice, alice, nil, doc, recents.KindEdit, nil))
	first := getRow(t, alice, doc.ID())
	require.Equal(t, "before.txt", first.Name)

	// The same activity on a renamed item is neither hidden by the memory of
	// the stored activities nor by the minimal interval, and moves no date.
	name := "after.txt"
	renamed, err := vfs.ModifyFileMetadata(alice.VFS(), doc, &vfs.DocPatch{Name: &name})
	require.NoError(t, err)

	require.NoError(t, recents.RecordActivity(alice, alice, nil, renamed, recents.KindEdit, nil))
	row := getRow(t, alice, doc.ID())
	assert.Equal(t, "after.txt", row.Name)
	assert.Equal(t, "/after.txt", row.Path)
	assert.Equal(t, first.LastEditedAt, row.LastEditedAt)
	assert.NotEqual(t, first.DocRev, row.DocRev)
}

func TestRecordActivitySkipsWhatItJustStored(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "remembered.txt")

	require.NoError(t, recents.RecordActivity(alice, alice, nil, doc, recents.KindEdit, nil))
	row := getRow(t, alice, doc.ID())
	require.NoError(t, couchdb.DeleteDoc(alice, &row))

	// The same activity within the minimal interval is known to be stored:
	// CouchDB is not read, and the deleted row is not recreated.
	require.NoError(t, recents.RecordActivity(alice, alice, nil, doc, recents.KindEdit, nil))
	assert.Empty(t, allRows(t, alice))

	recents.ResetActivityCacheForTest()
	require.NoError(t, recents.RecordActivity(alice, alice, nil, doc, recents.KindEdit, nil))
	assert.NotEmpty(t, getRow(t, alice, doc.ID()).LastEditedAt)
}

func TestRecordActivityUnderConcurrency(t *testing.T) {
	alice, _ := setupRecordTest(t)
	doc := createFile(t, alice, "concurrent.txt")

	// The requests race on the creation of the row, and of its database on
	// the first activity of the instance.
	var g errgroup.Group
	for range 8 {
		g.Go(func() error {
			return recents.RecordActivity(alice, alice, nil, doc, recents.KindEdit, nil)
		})
	}
	require.NoError(t, g.Wait())

	assert.Len(t, allRows(t, alice), 1)
}

func TestEnabledReadsTheInstanceFlags(t *testing.T) {
	for _, tc := range []struct {
		value    interface{}
		expected bool
	}{
		{true, true},
		{false, false},
		{nil, false},
		{"true", false},
	} {
		inst := &instance.Instance{
			DocID:        uuid.Must(uuid.NewV7()).String(),
			FeatureFlags: map[string]interface{}{"drive.recents.enabled": tc.value},
		}
		assert.Equal(t, tc.expected, recents.Enabled(inst), "flag value %#v", tc.value)
	}
}

func TestEnabledReadsTheOtherFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("CouchDB is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	contextName := "recents-" + uuid.Must(uuid.NewV7()).String()
	inst := &instance.Instance{
		DocID:       uuid.Must(uuid.NewV7()).String(),
		ContextName: contextName,
	}
	assert.False(t, recents.Enabled(inst), "the flag is off by default")

	flags := &couchdb.JSONDoc{
		Type: consts.Settings,
		M: map[string]interface{}{
			"_id": consts.ContextFlagsSettingsID + "." + contextName,
			"drive.recents.enabled": []interface{}{
				map[string]interface{}{"ratio": 1.0, "value": true},
			},
		},
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(prefixer.GlobalPrefixer, flags))
	t.Cleanup(func() { _ = couchdb.DeleteDoc(prefixer.GlobalPrefixer, flags) })

	assert.True(t, recents.Enabled(inst), "a change of the context flag applies at once")

	inst.FeatureFlags = map[string]interface{}{"drive.recents.enabled": false}
	assert.False(t, recents.Enabled(inst), "the flag of the instance wins over the context")
}
