package office

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupOfficeRecents(t *testing.T) *instance.Instance {
	t.Helper()
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.URL = &url.URL{Scheme: "file", Host: "localhost", Path: t.TempDir()}
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	return inst
}

func createOfficeFile(t *testing.T, inst *instance.Instance, name string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
		"application/vnd.oasis.opendocument.text", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return doc
}

func TestSessionRecordsTheOpenThenTheSave(t *testing.T) {
	inst := setupOfficeRecents(t)
	file := createOfficeFile(t, inst, "edited.docx")
	key, err := GetStore().AddDoc(inst, ConflictDetector{ID: file.ID(), Rev: file.Rev(), MD5Sum: file.MD5Sum})
	require.NoError(t, err)

	OpenSession(inst, inst, file, false, key)

	row := testutils.FindRecentsRow(t, inst, file.ID())
	require.NotNil(t, row)
	assert.NotEmpty(t, row.LastViewedAt)
	assert.Empty(t, row.LastEditedAt)

	// The opener becomes a participant, so the save callback credits them.
	participants, err := GetStore().GetParticipants(inst, key)
	require.NoError(t, err)
	require.Len(t, participants, 1)
	assert.Equal(t, inst.Domain, participants[0].Domain)

	recordSaveActivity(inst, file.ID(), participants)

	row = testutils.FindRecentsRow(t, inst, file.ID())
	require.NotNil(t, row)
	assert.NotEmpty(t, row.LastEditedAt)
}

func TestForceSaveCreditsAnOpeningDuringTheDownload(t *testing.T) {
	inst := setupOfficeRecents(t)
	file := createOfficeFile(t, inst, "opened-while-saving.docx")
	key, err := GetStore().AddDoc(inst, ConflictDetector{ID: file.ID(), Rev: file.Rev(), MD5Sum: file.MD5Sum})
	require.NoError(t, err)

	// The person opens the document while the save downloads its content.
	docserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		OpenSession(inst, inst, file, false, key)
		_, _ = w.Write([]byte("saved content"))
	}))
	defer docserver.Close()

	require.NoError(t, forceSaveFile(inst, key, docserver.URL))

	row := testutils.FindRecentsRow(t, inst, file.ID())
	require.NotNil(t, row)
	assert.NotEmpty(t, row.LastEditedAt)
	participants, err := GetStore().GetParticipants(inst, key)
	require.NoError(t, err)
	assert.Len(t, participants, 1)
}

func TestSaveWithoutParticipantRecordsNothing(t *testing.T) {
	inst := setupOfficeRecents(t)
	file := createOfficeFile(t, inst, "untouched.docx")

	recordSaveActivity(inst, file.ID(), nil)

	assert.Nil(t, testutils.FindRecentsRow(t, inst, file.ID()))
}
