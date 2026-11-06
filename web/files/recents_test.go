package files

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func findRecentsRow(t *testing.T, inst *instance.Instance, id string) *recents.FileDirActivity {
	t.Helper()
	var doc recents.FileDirActivity
	err := couchdb.GetDoc(inst, consts.FilesRecents, id, &doc)
	if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
		return nil
	}
	require.NoError(t, err)
	return &doc
}

func createRecentsFile(t *testing.T, inst *instance.Instance, name string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
		"text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return doc
}

func createRecentsApp(t *testing.T, inst *instance.Instance, slug string) {
	t.Helper()
	_, err := permission.CreateWebappSet(inst, slug, permission.Set{
		permission.Rule{Type: consts.Files, Verbs: permission.ALL},
	}, "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = permission.DestroyWebapp(inst, slug) })
}

func createRecentsKonnector(t *testing.T, inst *instance.Instance, slug string) {
	t.Helper()
	_, err := permission.CreateKonnectorSet(inst, slug, permission.Set{
		permission.Rule{Type: consts.Files, Verbs: permission.ALL},
	}, "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = permission.DestroyKonnector(inst, slug) })
}

func recentsTestServer(t *testing.T, inst *instance.Instance, sess *session.Session) *httptest.Server {
	t.Helper()
	handler := echo.New()
	handler.HTTPErrorHandler = errors.ErrorHandler
	group := handler.Group("/files", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("instance", inst)
			c.Set("session", sess)
			return next(c)
		}
	})
	Routes(group)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func setupRecentsCapture(t *testing.T) (*testutils.TestSetup, *instance.Instance, *session.Session) {
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

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	sess, err := session.New(inst, session.LongRun, "")
	require.NoError(t, err)
	createRecentsApp(t, inst, "drive")
	return setup, inst, sess
}

func TestRecentsCapture(t *testing.T) {
	setup, inst, sess := setupRecentsCapture(t)
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	createRecentsKonnector(t, inst, "konnector")

	ts := recentsTestServer(t, inst, sess)
	e := testutils.CreateTestClient(t, ts.URL)

	driveToken := inst.BuildAppToken("drive", sess.ID())
	flagshipToken := func() string {
		created, token := setup.GetTestClient("*")
		client, err := oauth.FindClient(inst, created.ClientID)
		require.NoError(t, err)
		require.NoError(t, client.SetFlagship(inst))
		return token
	}
	createWith := func(token, dirID, name, typ, metadata string) string {
		req := e.POST("/files/"+dirID).
			WithQuery("Name", name).
			WithQuery("Type", typ).
			WithHeader("Authorization", "Bearer "+token)
		if metadata != "" {
			req = req.WithQuery("Metadata", metadata)
		}
		if typ == consts.FileType {
			req = req.WithHeader("Content-Type", "text/plain").WithBytes([]byte("hello"))
		}
		return req.Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()
	}
	overwrite := func(fileID, token, content string) {
		e.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte(content)).
			Expect().Status(200)
	}
	rename := func(fileID, name string) {
		e.PATCH("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+driveToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{"data":{"type":"io.cozy.files","id":"` + fileID +
				`","attributes":{"name":"` + name + `"}}}`)).
			Expect().Status(200)
	}

	t.Run("CreatedFileWithItsMetadata", func(t *testing.T) {
		fileID := createWith(driveToken, "", "created.txt", consts.FileType, "")

		doc, err := inst.VFS().FileByID(fileID)
		require.NoError(t, err)
		row := findRecentsRow(t, inst, fileID)
		require.NotNil(t, row)
		assert.Equal(t, "created.txt", row.Name)
		assert.Equal(t, consts.FileType, row.Type)
		assert.Equal(t, "text/plain", row.Mime)
		assert.Equal(t, "text", row.Class)
		assert.Equal(t, "5", row.Size)
		assert.Equal(t, doc.UpdatedAt.Format(time.RFC3339Nano), row.UpdatedAt)
		assert.Equal(t, consts.RootDirID, row.DirID)
		assert.Equal(t, "/created.txt", row.Path)
		assert.Empty(t, row.DriveID)
		assert.Empty(t, row.OwnerDomain)
		assert.NotEmpty(t, row.LastEditedAt)
		assert.Empty(t, row.LastViewedAt)
	})

	t.Run("CreatedFolderWithItsMetadata", func(t *testing.T) {
		parentID := createWith(driveToken, "", "a-parent", consts.DirType, "")
		dirID := createWith(driveToken, parentID, "a-folder", consts.DirType, "")

		row := findRecentsRow(t, inst, dirID)
		require.NotNil(t, row)
		assert.Equal(t, "a-folder", row.Name)
		assert.Equal(t, consts.DirType, row.Type)
		assert.Empty(t, row.Mime)
		assert.Empty(t, row.Size)
		assert.Equal(t, parentID, row.DirID)
		assert.Equal(t, "/a-parent/a-folder", row.Path)
		assert.NotEmpty(t, row.LastEditedAt)
	})

	t.Run("OverwriteKeepsTheContentUpToDate", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "overwritten.txt")

		overwrite(doc.ID(), driveToken, "new content")
		row := findRecentsRow(t, inst, doc.ID())
		require.NotNil(t, row)
		assert.Equal(t, "11", row.Size)
		assert.NotEmpty(t, row.LastEditedAt)

		overwrite(doc.ID(), driveToken, "newer")
		assert.Equal(t, "5", findRecentsRow(t, inst, doc.ID()).Size)
	})

	t.Run("RevertWritesTheMetadataOfTheVersion", func(t *testing.T) {
		fileID := createWith(driveToken, "", "reverted.txt", consts.FileType, "")
		overwrite(fileID, driveToken, "new content")
		row := findRecentsRow(t, inst, fileID)
		require.NotNil(t, row)
		require.Equal(t, "11", row.Size)

		versionID := e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.relationships.old_versions.data[0].id").
			String().NotEmpty().Raw()

		e.POST("/files/revert/"+versionID).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)

		assert.Equal(t, "5", findRecentsRow(t, inst, fileID).Size)
	})

	t.Run("RestoreRecordsWhereTheItemCameBack", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "restored.txt")

		e.DELETE("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)
		assert.Nil(t, findRecentsRow(t, inst, doc.ID()), "trashing must not record activity")

		e.POST("/files/trash/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)
		row := findRecentsRow(t, inst, doc.ID())
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastEditedAt)
		assert.Equal(t, "/restored.txt", row.Path)
	})

	t.Run("RenameRecordsNothingAndTheNextActivityUpdatesTheName", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "before.txt")
		rename(doc.ID(), "untracked.txt")
		assert.Nil(t, findRecentsRow(t, inst, doc.ID()), "a rename is not an activity")

		overwrite(doc.ID(), driveToken, "first save")
		first := findRecentsRow(t, inst, doc.ID())
		require.NotNil(t, first)
		assert.Equal(t, "untracked.txt", first.Name)

		rename(doc.ID(), "after.txt")
		overwrite(doc.ID(), driveToken, "second save")
		row := findRecentsRow(t, inst, doc.ID())
		assert.Equal(t, "after.txt", row.Name)
		assert.Equal(t, "/after.txt", row.Path)
		assert.Equal(t, first.LastEditedAt, row.LastEditedAt,
			"two activities less than a minute apart keep their date")
	})

	t.Run("NothingForATokenThatIsNotAPerson", func(t *testing.T) {
		_, oauthToken := setup.GetTestClient(consts.Files)
		for name, token := range map[string]string{
			"konnector":    inst.BuildKonnectorToken("konnector"),
			"service":      inst.BuildAppToken("drive", ""),
			"other client": oauthToken,
		} {
			fileID := createWith(token, "", "by-"+name+".txt", consts.FileType, "")
			assert.Nil(t, findRecentsRow(t, inst, fileID), name)
		}
	})

	t.Run("NothingOnFlagshipAutomaticWrites", func(t *testing.T) {
		token := flagshipToken()

		fileID := createWith(token, "", "IMG_0001.jpg", consts.FileType, `{"backupDeviceIds":["device-1"]}`)
		assert.Nil(t, findRecentsRow(t, inst, fileID), "photo backup")

		dirID := createWith(token, "", "Mobile backup", consts.DirType, "")
		assert.Nil(t, findRecentsRow(t, inst, dirID), "folder of an automatic write")

		for _, path := range []string{"/Settings/Logs", "/Settings/AALogs/20260101_120000_000"} {
			dir, err := vfs.MkdirAll(inst.VFS(), path)
			require.NoError(t, err)

			fileID := createWith(token, dir.ID(), "trace.html", consts.FileType, "")
			assert.Nil(t, findRecentsRow(t, inst, fileID), path)

			// The exception is for the flagship app only.
			fileID = createWith(driveToken, dir.ID(), "by-drive.html", consts.FileType, "")
			assert.NotNil(t, findRecentsRow(t, inst, fileID), path)
		}
	})

	t.Run("FlagshipUploadCounts", func(t *testing.T) {
		// An upload from the OS share sheet of the mobile app.
		fileID := createWith(flagshipToken(), "", "shared-from-phone.txt", consts.FileType, "")
		assert.NotNil(t, findRecentsRow(t, inst, fileID))
	})

	t.Run("WholeScopeOAuthUploadCounts", func(t *testing.T) {
		// The flagship app gets the "*" scope without being certified when
		// its context skips the certification.
		flagshipCfg := config.GetConfig().Flagship.Contexts
		config.GetConfig().Flagship.Contexts = map[string]interface{}{
			config.DefaultInstanceContext: map[string]interface{}{"skip_certification": true},
		}
		t.Cleanup(func() { config.GetConfig().Flagship.Contexts = flagshipCfg })
		_, token := setup.GetTestClient("*")

		fileID := createWith(token, "", "uncertified.txt", consts.FileType, "")
		assert.NotNil(t, findRecentsRow(t, inst, fileID))
	})

	t.Run("NothingOnReads", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "read.txt")

		e.GET("/files/download/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)
		e.GET("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)
		assert.Nil(t, findRecentsRow(t, inst, doc.ID()))
	})

	t.Run("NothingOnFailedWrite", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "failed.txt")

		e.PUT("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			WithHeader("Content-Type", "text/plain").
			WithHeader("If-Match", "1-not-the-revision").
			WithBytes([]byte("nope")).
			Expect().Status(http.StatusPreconditionFailed)
		assert.Nil(t, findRecentsRow(t, inst, doc.ID()))
	})

	t.Run("NothingOnAFixedID", func(t *testing.T) {
		// Making the root folder succeeds, and returns its fixed ID.
		e.POST("/files/").
			WithQuery("Type", consts.DirType).
			WithQuery("Path", "/").
			WithQuery("Recursive", "true").
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(201)
		assert.Nil(t, findRecentsRow(t, inst, consts.RootDirID))
	})
}

func TestRecentsFeatureFlag(t *testing.T) {
	_, inst, sess := setupRecentsCapture(t)

	ts := recentsTestServer(t, inst, sess)
	e := testutils.CreateTestClient(t, ts.URL)
	token := inst.BuildAppToken("drive", sess.ID())

	createFile := func(name string) string {
		return e.POST("/files/").
			WithQuery("Name", name).
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("hello")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()
	}

	fileID := createFile("flagged-off.txt")
	assert.Nil(t, findRecentsRow(t, inst, fileID), "no capture while the flag is off")

	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	fileID = createFile("flagged-on.txt")
	assert.NotNil(t, findRecentsRow(t, inst, fileID))
}
