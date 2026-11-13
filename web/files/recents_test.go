package files

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestRecentsCapture(t *testing.T) {
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
	_, err = permission.CreateKonnectorSet(inst, "konnector", permission.Set{
		permission.Rule{Type: consts.Files, Verbs: permission.ALL},
	}, "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = permission.DestroyKonnector(inst, "konnector") })

	ts := setup.GetTestServer("/files", Routes, func(r *echo.Echo) *echo.Echo {
		r.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Set("session", sess)
				return next(c)
			}
		})
		return r
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	e := testutils.CreateTestClient(t, ts.URL)

	driveToken := inst.BuildAppToken("drive", sess.ID())
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

	t.Run("NothingWhileTheFlagIsOff", func(t *testing.T) {
		fileID := createWith(driveToken, "", "flagged-off.txt", consts.FileType, "")
		assert.Nil(t, testutils.FindRecentsRow(t, inst, fileID))
	})

	testutils.WithFlag(t, inst, "drive.recents.enabled", true)

	// The fields of the row that the model fills from the item alone are checked
	// by its own tests.
	t.Run("CreatedItemsWithTheirMetadata", func(t *testing.T) {
		parentID := createWith(driveToken, "", "a-parent", consts.DirType, "")
		dirID := createWith(driveToken, parentID, "a-folder", consts.DirType, "")
		fileID := createWith(driveToken, dirID, "created.txt", consts.FileType, "")

		row := testutils.FindRecentsRow(t, inst, dirID)
		require.NotNil(t, row)
		assert.Equal(t, parentID, row.DirID)
		assert.Equal(t, "/a-parent/a-folder", row.Path)
		assert.NotEmpty(t, row.LastEditedAt)

		row = testutils.FindRecentsRow(t, inst, fileID)
		require.NotNil(t, row)
		assert.Equal(t, "5", row.Size)
		assert.Equal(t, dirID, row.DirID)
		assert.Equal(t, "/a-parent/a-folder/created.txt", row.Path)
		assert.NotEmpty(t, row.LastEditedAt)
	})

	t.Run("OverwriteKeepsTheContentUpToDate", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "overwritten.txt")

		overwrite(doc.ID(), driveToken, "new content")
		row := testutils.FindRecentsRow(t, inst, doc.ID())
		require.NotNil(t, row)
		assert.Equal(t, "11", row.Size)
		assert.NotEmpty(t, row.LastEditedAt)

		overwrite(doc.ID(), driveToken, "newer")
		assert.Equal(t, "5", testutils.FindRecentsRow(t, inst, doc.ID()).Size)
	})

	t.Run("RevertWritesTheMetadataOfTheVersion", func(t *testing.T) {
		fileID := createWith(driveToken, "", "reverted.txt", consts.FileType, "")
		overwrite(fileID, driveToken, "new content")
		row := testutils.FindRecentsRow(t, inst, fileID)
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

		assert.Equal(t, "5", testutils.FindRecentsRow(t, inst, fileID).Size)
	})

	t.Run("RestoreRecordsWhereTheItemCameBack", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "restored.txt")

		e.DELETE("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)
		assert.Nil(t, testutils.FindRecentsRow(t, inst, doc.ID()), "trashing must not record activity")

		e.POST("/files/trash/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)
		row := testutils.FindRecentsRow(t, inst, doc.ID())
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastEditedAt)
		assert.Equal(t, "/restored.txt", row.Path)
	})

	t.Run("RenameRecordsNothingAndTheNextActivityHasTheName", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "before.txt")
		e.PATCH("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{"data":{"type":"io.cozy.files","id":"` + doc.ID() +
				`","attributes":{"name":"untracked.txt"}}}`)).
			Expect().Status(200)
		assert.Nil(t, testutils.FindRecentsRow(t, inst, doc.ID()), "a rename is not an activity")

		overwrite(doc.ID(), driveToken, "first save")
		row := testutils.FindRecentsRow(t, inst, doc.ID())
		require.NotNil(t, row)
		assert.Equal(t, "untracked.txt", row.Name)
	})

	t.Run("NothingForATokenThatIsNotAPerson", func(t *testing.T) {
		_, oauthToken := setup.GetTestClient(consts.Files)
		for name, token := range map[string]string{
			"konnector":    inst.BuildKonnectorToken("konnector"),
			"service":      inst.BuildAppToken("drive", ""),
			"other client": oauthToken,
		} {
			fileID := createWith(token, "", "by-"+name+".txt", consts.FileType, "")
			assert.Nil(t, testutils.FindRecentsRow(t, inst, fileID), name)
		}
	})

	t.Run("NothingOnFlagshipAutomaticWrites", func(t *testing.T) {
		created, token := setup.GetTestClient("*")
		client, err := oauth.FindClient(inst, created.ClientID)
		require.NoError(t, err)
		require.NoError(t, client.SetFlagship(inst))

		fileID := createWith(token, "", "IMG_0001.jpg", consts.FileType, `{"backupDeviceIds":["device-1"]}`)
		assert.Nil(t, testutils.FindRecentsRow(t, inst, fileID), "photo backup")

		dirID := createWith(token, "", "Mobile backup", consts.DirType, "")
		assert.Nil(t, testutils.FindRecentsRow(t, inst, dirID), "folder of an automatic write")

		for _, path := range []string{"/Settings/Logs", "/Settings/AALogs/20260101_120000_000"} {
			dir, err := vfs.MkdirAll(inst.VFS(), path)
			require.NoError(t, err)

			fileID := createWith(token, dir.ID(), "trace.html", consts.FileType, "")
			assert.Nil(t, testutils.FindRecentsRow(t, inst, fileID), path)

			// The exception is for the flagship app only.
			fileID = createWith(driveToken, dir.ID(), "by-drive.html", consts.FileType, "")
			assert.NotNil(t, testutils.FindRecentsRow(t, inst, fileID), path)
		}

		// An upload from the OS share sheet of the mobile app is a write of the
		// person.
		fileID = createWith(token, "", "shared-from-phone.txt", consts.FileType, "")
		assert.NotNil(t, testutils.FindRecentsRow(t, inst, fileID), "upload of the person")
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
		assert.NotNil(t, testutils.FindRecentsRow(t, inst, fileID))
	})

	t.Run("NothingOnReadsAndFailedWrites", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "untouched.txt")

		e.GET("/files/download/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)
		e.GET("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			Expect().Status(200)
		e.PUT("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+driveToken).
			WithHeader("Content-Type", "text/plain").
			WithHeader("If-Match", "1-not-the-revision").
			WithBytes([]byte("nope")).
			Expect().Status(http.StatusPreconditionFailed)
		assert.Nil(t, testutils.FindRecentsRow(t, inst, doc.ID()))
	})
}
