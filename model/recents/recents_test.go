package recents_test

import (
	"encoding/json"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
