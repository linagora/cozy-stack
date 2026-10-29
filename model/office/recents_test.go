package office_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/office"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testOutboxSecret = "test-outbox-secret"

func TestConflictDetectorJSONKeepsParticipants(t *testing.T) {
	// The redis store keeps sessions as JSON: the private participants must
	// survive a round trip, in the stored format.
	stored := `{"ID":"file-1","Rev":"1-abc","MD5Sum":"AQID","Participants":["alice.example.net","bob.example.net"]}`

	var detector office.ConflictDetector
	require.NoError(t, json.Unmarshal([]byte(stored), &detector))
	assert.Equal(t, "file-1", detector.ID)
	assert.Equal(t, "1-abc", detector.Rev)
	assert.Equal(t, []byte{1, 2, 3}, detector.MD5Sum)
	assert.Equal(t, []string{"alice.example.net", "bob.example.net"}, detector.Participants())

	// The store marshals a value, not a pointer.
	marshaled, err := json.Marshal(detector)
	require.NoError(t, err)
	assert.JSONEq(t, stored, string(marshaled))
}

func signCallbackToken(t *testing.T, key string, status int, downloadURL string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"payload": map[string]interface{}{
			"key":    key,
			"status": status,
			"url":    downloadURL,
		},
	}).SignedString([]byte(testOutboxSecret))
	require.NoError(t, err)
	return token
}

func recentsRows(t *testing.T, inst *instance.Instance) []recents.FileDirActivity {
	t.Helper()
	var docs []recents.FileDirActivity
	require.NoError(t, couchdb.GetAllDocs(inst, consts.FilesRecents, &couchdb.AllDocsRequest{}, &docs))
	return docs
}

func findRecentsRow(t *testing.T, owner, src *instance.Instance, fileID string) *recents.FileDirActivity {
	t.Helper()
	key := recents.ResourceKey(src, fileID)
	rows := recentsRows(t, owner)
	for i := range rows {
		if rows[i].DocID == key {
			return &rows[i]
		}
	}
	return nil
}

func createOfficeFile(t *testing.T, inst *instance.Instance, name string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return doc
}

func TestOnlyOfficeRecents(t *testing.T) {
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
	savedOffice := config.GetConfig().Office
	config.GetConfig().Office = map[string]config.Office{
		config.DefaultInstanceContext: {
			OnlyOfficeURL: "http://documentserver.example",
			OutboxSecret:  testOutboxSecret,
		},
	}
	t.Cleanup(func() { config.GetConfig().Office = savedOffice })

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	opener := testutils.NewSetup(t, t.Name()+"opener").GetTestInstance()
	other := testutils.NewSetup(t, t.Name()+"other").GetTestInstance()
	testutils.WithFlag(t, opener, "drive.recents.enabled", true)

	docserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("edited content"))
	}))
	t.Cleanup(docserver.Close)

	filedoc, err := vfs.NewFileDoc("report.docx", consts.RootDirID, -1, nil,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(filedoc, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	fileID := filedoc.ID()

	t.Run("OpenSessionRecordsViewForOpener", func(t *testing.T) {
		key, err := office.GetStore().AddDoc(inst, office.ConflictDetector{
			ID:     fileID,
			Rev:    filedoc.Rev(),
			MD5Sum: filedoc.MD5Sum,
		})
		require.NoError(t, err)

		office.OpenSession(inst, opener, key)

		docs := recentsRows(t, opener)
		require.Len(t, docs, 1)
		assert.Equal(t, recents.ResourceKey(inst, fileID), docs[0].DocID)
		assert.NotEmpty(t, docs[0].LastViewedAt)
		assert.Empty(t, docs[0].LastEditedAt)
		assert.Empty(t, recentsRows(t, inst), "the row belongs to the opener, not the file owner")
		assert.Empty(t, recentsRows(t, other), "an activity writes only the opener's instance")
	})

	t.Run("StaleCallbackCreatesNothing", func(t *testing.T) {
		url := docserver.URL + "/stale"
		err := office.Callback(inst, office.CallbackParameters{
			Key:    "unknown-key",
			Status: office.StatusReadyForSaving,
			URL:    url,
			Token:  signCallbackToken(t, "unknown-key", office.StatusReadyForSaving, url),
		})
		require.ErrorIs(t, err, office.ErrInvalidKey)
		assert.Empty(t, recentsRows(t, inst))
	})

	t.Run("SaveCallbackRecordsEditForOpener", func(t *testing.T) {
		key, err := office.GetStore().GetSecretByID(inst, fileID)
		require.NoError(t, err)
		require.NotEmpty(t, key)

		downloadURL := docserver.URL + "/save"
		require.NoError(t, office.Callback(inst, office.CallbackParameters{
			Key:    key,
			Status: office.StatusReadyForSaving,
			URL:    downloadURL,
			Token:  signCallbackToken(t, key, office.StatusReadyForSaving, downloadURL),
		}))

		docs := recentsRows(t, opener)
		require.Len(t, docs, 1)
		assert.NotEmpty(t, docs[0].LastEditedAt)
		assert.NotEmpty(t, docs[0].LastViewedAt, "the session opening must be kept")
		assert.Equal(t, docs[0].LastActivityAt, max(docs[0].LastEditedAt, docs[0].LastViewedAt))
		assert.Empty(t, recentsRows(t, inst), "the row belongs to the opener, not the file owner")
		assert.Empty(t, recentsRows(t, other), "an activity writes only the opener's instance")
	})

	t.Run("AbsentOpenerLosesActivity", func(t *testing.T) {
		ghost, err := vfs.NewFileDoc("ghost.docx", consts.RootDirID, -1, nil,
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := inst.VFS().CreateFile(ghost, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		key, err := office.GetStore().AddDoc(inst, office.ConflictDetector{
			ID:     ghost.ID(),
			Rev:    ghost.Rev(),
			MD5Sum: ghost.MD5Sum,
		})
		require.NoError(t, err)

		// The opener's instance is not on this stack: no person to credit.
		office.OpenSession(inst, nil, key)

		downloadURL := docserver.URL + "/ghost"
		require.NoError(t, office.Callback(inst, office.CallbackParameters{
			Key:    key,
			Status: office.StatusReadyForSaving,
			URL:    downloadURL,
			Token:  signCallbackToken(t, key, office.StatusReadyForSaving, downloadURL),
		}), "a lost activity must not fail the save")
		assertNoRecentsRow(t, recents.ResourceKey(inst, ghost.ID()), inst, opener, other)
	})

	t.Run("UnknownOpenerInstanceLosesActivity", func(t *testing.T) {
		ghost, err := vfs.NewFileDoc("deleted-opener.docx", consts.RootDirID, -1, nil,
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := inst.VFS().CreateFile(ghost, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		key, err := office.GetStore().AddDoc(inst, office.ConflictDetector{
			ID:     ghost.ID(),
			Rev:    ghost.Rev(),
			MD5Sum: ghost.MD5Sum,
		})
		require.NoError(t, err)
		require.NoError(t, office.GetStore().AddParticipant(inst, key, "ghost.example.net"))

		downloadURL := docserver.URL + "/deleted-opener"
		require.NoError(t, office.Callback(inst, office.CallbackParameters{
			Key:    key,
			Status: office.StatusReadyForSaving,
			URL:    downloadURL,
			Token:  signCallbackToken(t, key, office.StatusReadyForSaving, downloadURL),
		}), "a lost activity must not fail the save")
		assertNoRecentsRow(t, recents.ResourceKey(inst, ghost.ID()), inst, opener, other)
	})

	t.Run("SaveCreditsEveryParticipant", func(t *testing.T) {
		testutils.WithFlag(t, other, "drive.recents.enabled", true)
		doc := createOfficeFile(t, inst, "shared.docx")
		key, err := office.GetStore().AddDoc(inst, office.ConflictDetector{
			ID:     doc.ID(),
			Rev:    doc.Rev(),
			MD5Sum: doc.MD5Sum,
		})
		require.NoError(t, err)

		office.OpenSession(inst, opener, key)
		office.OpenSession(inst, other, key)

		aliceRow := findRecentsRow(t, opener, inst, doc.ID())
		bobRow := findRecentsRow(t, other, inst, doc.ID())
		require.NotNil(t, aliceRow, "Alice's open must be in her recents")
		require.NotNil(t, bobRow, "Bob's open must be in his recents")
		assert.NotEmpty(t, aliceRow.LastViewedAt)
		assert.NotEmpty(t, bobRow.LastViewedAt)
		assert.Empty(t, aliceRow.LastEditedAt, "no save yet")
		assert.Empty(t, bobRow.LastEditedAt, "no save yet")

		downloadURL := docserver.URL + "/shared"
		require.NoError(t, office.Callback(inst, office.CallbackParameters{
			Key:    key,
			Status: office.StatusReadyForSaving,
			URL:    downloadURL,
			Token:  signCallbackToken(t, key, office.StatusReadyForSaving, downloadURL),
		}), "crediting every participant must not fail the save")

		aliceRow = findRecentsRow(t, opener, inst, doc.ID())
		bobRow = findRecentsRow(t, other, inst, doc.ID())
		require.NotNil(t, aliceRow)
		require.NotNil(t, bobRow)
		assert.NotEmpty(t, aliceRow.LastEditedAt, "Alice edited the document: the save must reach her recents")
		assert.NotEmpty(t, bobRow.LastEditedAt, "Bob edited the document: the save must reach his recents")
	})

	t.Run("OpenDuringForceSaveIsKept", func(t *testing.T) {
		testutils.WithFlag(t, other, "drive.recents.enabled", true)
		doc := createOfficeFile(t, inst, "force-saved.docx")
		key, err := office.GetStore().AddDoc(inst, office.ConflictDetector{
			ID:     doc.ID(),
			Rev:    doc.Rev(),
			MD5Sum: doc.MD5Sum,
		})
		require.NoError(t, err)
		office.OpenSession(inst, opener, key)

		// Bob opens the session while the force-save downloads the content.
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			office.OpenSession(inst, other, key)
			_, _ = w.Write([]byte("force-saved content"))
		}))
		t.Cleanup(slow.Close)

		downloadURL := slow.URL + "/force-save"
		require.NoError(t, office.Callback(inst, office.CallbackParameters{
			Key:    key,
			Status: office.StatusForceSaveRequested,
			URL:    downloadURL,
			Token:  signCallbackToken(t, key, office.StatusForceSaveRequested, downloadURL),
		}))

		detector, err := office.GetStore().GetDoc(inst, key)
		require.NoError(t, err)
		require.NotNil(t, detector)
		assert.ElementsMatch(t, []string{opener.Domain, other.Domain}, detector.Participants(),
			"an open during the force-save must stay a participant")
		assert.NotEqual(t, doc.Rev(), detector.Rev, "the force-save moves the session to the new revision")
	})
}

// assertNoRecentsRow checks that none of the instances holds a recents row
// with the given key.
func assertNoRecentsRow(t *testing.T, key string, targets ...*instance.Instance) {
	t.Helper()
	for _, target := range targets {
		for _, row := range recentsRows(t, target) {
			assert.NotEqual(t, key, row.DocID, "no row when the actor's instance is not on this stack")
		}
	}
}

func TestOnlyOfficeRecentsFlagDisabled(t *testing.T) {
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
	savedOffice := config.GetConfig().Office
	config.GetConfig().Office = map[string]config.Office{
		config.DefaultInstanceContext: {
			OnlyOfficeURL: "http://documentserver.example",
			OutboxSecret:  testOutboxSecret,
		},
	}
	t.Cleanup(func() { config.GetConfig().Office = savedOffice })

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	opener := testutils.NewSetup(t, t.Name()+"opener").GetTestInstance()

	docserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("edited content"))
	}))
	t.Cleanup(docserver.Close)

	newFileDoc := func(name string) *vfs.FileDoc {
		doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := inst.VFS().CreateFile(doc, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return doc
	}

	t.Run("OpenSessionRecordsNothing", func(t *testing.T) {
		doc := newFileDoc("open-disabled.docx")
		key, err := office.GetStore().AddDoc(inst, office.ConflictDetector{
			ID:     doc.ID(),
			Rev:    doc.Rev(),
			MD5Sum: doc.MD5Sum,
		})
		require.NoError(t, err)

		office.OpenSession(inst, opener, key)

		assert.Empty(t, recentsRows(t, opener), "no capture while the flag is off")
	})

	t.Run("SaveCallbackRecordsNothing", func(t *testing.T) {
		doc := newFileDoc("save-disabled.docx")
		key, err := office.GetStore().AddDoc(inst, office.ConflictDetector{
			ID:     doc.ID(),
			Rev:    doc.Rev(),
			MD5Sum: doc.MD5Sum,
		})
		require.NoError(t, err)
		// A session opened while the flag was on.
		require.NoError(t, office.GetStore().AddParticipant(inst, key, opener.Domain))

		downloadURL := docserver.URL + "/save-disabled"
		require.NoError(t, office.Callback(inst, office.CallbackParameters{
			Key:    key,
			Status: office.StatusReadyForSaving,
			URL:    downloadURL,
			Token:  signCallbackToken(t, key, office.StatusReadyForSaving, downloadURL),
		}), "a lost activity must not fail the save")
		assertNoRecentsRow(t, recents.ResourceKey(inst, doc.ID()), inst, opener)
	})
}
