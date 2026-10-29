package files

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testActorHeader makes the request look like a share-interact write from a
// remote member of the named instance, exercising the member identity path
// without the full sharing machinery (that path is covered in web/sharings).
const testActorHeader = "X-Test-Actor"

func recentsRows(t *testing.T, inst *instance.Instance) []recents.FileDirActivity {
	t.Helper()
	var docs []recents.FileDirActivity
	require.NoError(t, couchdb.GetAllDocs(inst, consts.FilesRecents, &couchdb.AllDocsRequest{}, &docs))
	return docs
}

func findRecentsRow(t *testing.T, inst *instance.Instance, src *instance.Instance, fileID string) *recents.FileDirActivity {
	t.Helper()
	key := recents.ResourceKey(src, fileID)
	docs := recentsRows(t, inst)
	for i := range docs {
		if docs[i].DocID == key {
			return &docs[i]
		}
	}
	return nil
}

// ageRecentsRow moves the dates of a row two minutes back, past the minimal
// rewrite interval, so that the next activity is visible as a moved date.
func ageRecentsRow(t *testing.T, inst *instance.Instance, row *recents.FileDirActivity) *recents.FileDirActivity {
	t.Helper()
	aged := *row
	for _, stamp := range []*string{&aged.LastEditedAt, &aged.LastViewedAt, &aged.LastActivityAt} {
		if *stamp == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, *stamp)
		require.NoError(t, err)
		*stamp = at.Add(-2 * time.Minute).Format(time.RFC3339)
	}
	require.NoError(t, couchdb.UpdateDoc(inst, &aged))
	return &aged
}

func createRecentsFile(t *testing.T, inst *instance.Instance, name, mime string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
		mime, "text", time.Now(), false, false, false, nil)
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
			if domain := c.Request().Header.Get(testActorHeader); domain != "" {
				middlewares.ForcePermission(c, &permission.Permission{
					Type:        permission.TypeShareInteract,
					Permissions: permission.Set{permission.Rule{Type: consts.Files, Verbs: permission.ALL}},
				})
				middlewares.SetActor(c, &middlewares.Actor{
					Kind:   vfs.TrashedByKindMember,
					Domain: domain,
				})
			}
			return next(c)
		}
	})
	Routes(group)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
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
	member := testutils.NewSetup(t, t.Name()+"member").GetTestInstance()
	other := testutils.NewSetup(t, t.Name()+"other").GetTestInstance()
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	testutils.WithFlag(t, member, "drive.recents.enabled", true)

	sess, err := session.New(inst, session.LongRun, "")
	require.NoError(t, err)

	createRecentsApp(t, inst, "notes")
	createRecentsApp(t, inst, "drive")
	createRecentsKonnector(t, inst, "konnector")

	ts := recentsTestServer(t, inst, sess)
	e := testutils.CreateTestClient(t, ts.URL)

	appToken := func(slug string) string { return inst.BuildAppToken(slug, sess.ID()) }
	oauthToken := func() string {
		_, token := setup.GetTestClient(consts.Files)
		return token
	}

	t.Run("EditOnCreate", func(t *testing.T) {
		fileID := e.POST("/files/").
			WithQuery("Name", "created.txt").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			WithBytes([]byte("hello")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		row := findRecentsRow(t, inst, inst, fileID)
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastEditedAt)
		assert.Empty(t, row.LastViewedAt)
		assert.Equal(t, row.LastEditedAt, row.LastActivityAt)
		assert.Equal(t, recents.ResourceKey(inst, fileID), row.DocID)
		assert.Equal(t, inst.Domain+"/"+fileID, row.SourceLocator)
		assert.Equal(t, 1, row.SchemaVersion)
	})

	t.Run("EditOnOverwrite", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "overwritten.txt", "text/plain")

		e.PUT("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			WithBytes([]byte("new content")).
			Expect().Status(200)

		row := findRecentsRow(t, inst, inst, doc.ID())
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastEditedAt)
	})

	t.Run("EditOnRestore", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "restored.txt", "text/plain")

		e.DELETE("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			Expect().Status(200)
		assert.Nil(t, findRecentsRow(t, inst, inst, doc.ID()), "trashing must not record activity")

		e.POST("/files/trash/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			Expect().Status(200)
		row := findRecentsRow(t, inst, inst, doc.ID())
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastEditedAt)
	})

	t.Run("EditOnRevert", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "reverted.txt", "text/plain")

		e.PUT("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			WithBytes([]byte("new content")).
			Expect().Status(200)
		before := findRecentsRow(t, inst, inst, doc.ID())
		require.NotNil(t, before)

		// Activities closer than the minimal rewrite interval do not move the
		// row: age it so that the revert is visible as a moved date.
		before = ageRecentsRow(t, inst, before)

		versionID := e.GET("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.relationships.old_versions.data[0].id").
			String().NotEmpty().Raw()

		e.POST("/files/revert/"+versionID).
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			Expect().Status(200)

		row := findRecentsRow(t, inst, inst, doc.ID())
		require.NotNil(t, row)
		assert.Greater(t, row.LastEditedAt, before.LastEditedAt,
			"reverting to an old version is a content edit: last_edited_at must move")
	})

	t.Run("EditOnDirCreate", func(t *testing.T) {
		dirID := e.POST("/files/").
			WithQuery("Name", "a-folder").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		row := findRecentsRow(t, inst, inst, dirID)
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastEditedAt)
		assert.Empty(t, row.LastViewedAt)
	})

	t.Run("NoActivityOnRename", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "before.txt", "text/plain")

		e.PATCH("/files/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{"data":{"type":"io.cozy.files","id":"` + doc.ID() + `","attributes":{"name":"after.txt"}}}`)).
			Expect().Status(200)

		assert.Nil(t, findRecentsRow(t, inst, inst, doc.ID()))
	})

	t.Run("NoActivityForKonnector", func(t *testing.T) {
		fileID := e.POST("/files/").
			WithQuery("Name", "by-konnector.txt").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+inst.BuildKonnectorToken("konnector")).
			WithBytes([]byte("hello")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		assert.Nil(t, findRecentsRow(t, inst, inst, fileID))
	})

	t.Run("MemberEditStaysOnTheirInstance", func(t *testing.T) {
		fileID := e.POST("/files/").
			WithQuery("Name", "shared.txt").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			WithBytes([]byte("from the owner")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		e.PUT("/files/"+fileID).
			WithHeader(testActorHeader, member.Domain).
			WithBytes([]byte("from the member")).
			Expect().Status(200)

		ownerRow := findRecentsRow(t, inst, inst, fileID)
		memberRow := findRecentsRow(t, member, inst, fileID)
		require.NotNil(t, ownerRow, "the owner edit must survive the member edit")
		require.NotNil(t, memberRow, "the member must get a row on their own instance")
		assert.Equal(t, recents.ResourceKey(inst, fileID), memberRow.DocID)
	})

	t.Run("MemberEditTouchesOnlyTheirInstance", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "shared-untouched.txt", "text/plain")

		e.PUT("/files/"+doc.ID()).
			WithHeader(testActorHeader, member.Domain).
			WithBytes([]byte("from the member")).
			Expect().Status(200)

		memberRow := findRecentsRow(t, member, inst, doc.ID())
		require.NotNil(t, memberRow, "the member must get a row on their own instance")
		assert.NotEmpty(t, memberRow.LastEditedAt)
		assert.Equal(t, recents.ResourceKey(inst, doc.ID()), memberRow.DocID)
		assert.Nil(t, findRecentsRow(t, inst, inst, doc.ID()),
			"the owner never touched the file: no row for them")
		assert.Empty(t, recentsRows(t, other),
			"an activity writes only the actor's instance, never other members")
	})

	t.Run("MemberEditWithoutLocalActorInstanceLosesActivity", func(t *testing.T) {
		doc := createRecentsFile(t, inst, "ghost-member.txt", "text/plain")

		e.PUT("/files/"+doc.ID()).
			WithHeader(testActorHeader, "ghost.example.net").
			WithBytes([]byte("from a member of another stack")).
			Expect().Status(200)

		assert.Nil(t, findRecentsRow(t, inst, inst, doc.ID()))
		assert.Nil(t, findRecentsRow(t, member, inst, doc.ID()))
		assert.Empty(t, recentsRows(t, other))
	})

	t.Run("NoOpenOnDownload", func(t *testing.T) {
		// A download is never an open, whoever reads: an editor read is the
		// same request.
		note := createRecentsFile(t, inst, "a-note.cozy-note", consts.NoteMimeType)
		drawing := createRecentsFile(t, inst, "a-drawing.excalidraw", "application/json")

		for _, token := range []string{
			appToken("drive"),
			appToken("notes"),
			oauthToken(),
			inst.BuildKonnectorToken("konnector"),
		} {
			for _, id := range []string{note.ID(), drawing.ID()} {
				e.GET("/files/download/"+id).
					WithHeader("Authorization", "Bearer "+token).
					Expect().Status(200)
			}
		}
		assert.Nil(t, findRecentsRow(t, inst, inst, note.ID()))
		assert.Nil(t, findRecentsRow(t, inst, inst, drawing.ID()))
	})

	t.Run("NoOpenOnMetadataRead", func(t *testing.T) {
		note := createRecentsFile(t, inst, "metadata-read.cozy-note", consts.NoteMimeType)

		e.GET("/files/"+note.ID()).
			WithHeader("Authorization", "Bearer "+appToken("notes")).
			Expect().Status(200)
		assert.Nil(t, findRecentsRow(t, inst, inst, note.ID()))
	})

	t.Run("NoActivityOnFailedWrite", func(t *testing.T) {
		e.PUT("/files/missing-file-id").
			WithHeader("Authorization", "Bearer "+appToken("drive")).
			WithBytes([]byte("nope")).
			Expect().Status(http.StatusNotFound)
		assert.Nil(t, findRecentsRow(t, inst, inst, "missing-file-id"))
	})
}

func TestRecentsFeatureFlag(t *testing.T) {
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

	ts := recentsTestServer(t, inst, sess)
	e := testutils.CreateTestClient(t, ts.URL)
	token := inst.BuildAppToken("drive", sess.ID())

	createFlaggedFile := func(name string) string {
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

	t.Run("FlagDisabledGatesCapture", func(t *testing.T) {
		fileID := createFlaggedFile("flagged-off.txt")
		assert.Nil(t, findRecentsRow(t, inst, inst, fileID), "no capture while the flag is off")
	})

	t.Run("FlagEnabledKeepsCurrentBehavior", func(t *testing.T) {
		testutils.WithFlag(t, inst, "drive.recents.enabled", true)

		fileID := createFlaggedFile("flagged-on.txt")
		row := findRecentsRow(t, inst, inst, fileID)
		require.NotNil(t, row)
		assert.NotEmpty(t, row.LastEditedAt)
	})
}
