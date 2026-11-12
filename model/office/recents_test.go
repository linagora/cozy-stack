package office

import (
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

	OpenSession(inst, inst, file, nil, key)

	row := testutils.FindRecentsRow(t, inst, file.ID())
	require.NotNil(t, row)
	assert.Equal(t, "/edited.docx", row.Path)
	assert.NotEmpty(t, row.LastViewedAt)
	assert.Empty(t, row.LastEditedAt)

	// The opener becomes a participant, so the save callback credits them.
	detector, err := GetStore().GetDoc(inst, key)
	require.NoError(t, err)
	require.Len(t, detector.Participants, 1)
	assert.Equal(t, inst.Domain, detector.Participants[0].Domain)

	recordSaveActivity(inst, detector)

	row = testutils.FindRecentsRow(t, inst, file.ID())
	require.NotNil(t, row)
	assert.NotEmpty(t, row.LastEditedAt)
}

func TestSaveWithoutParticipantRecordsNothing(t *testing.T) {
	inst := setupOfficeRecents(t)
	file := createOfficeFile(t, inst, "untouched.docx")
	saved := &ConflictDetector{ID: file.ID(), Rev: file.Rev(), MD5Sum: file.MD5Sum}

	recordSaveActivity(inst, saved)

	assert.Nil(t, testutils.FindRecentsRow(t, inst, file.ID()))
}
