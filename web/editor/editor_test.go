package editor

import (
	"net/http/httptest"
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
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadOnlyForSharedOpen(t *testing.T) {
	filesReadOnly := &permission.Permission{
		Permissions: permission.Set{
			{
				Type:  consts.Files,
				Verbs: permission.VerbSet{permission.GET: struct{}{}},
			},
		},
	}
	filesWritable := &permission.Permission{
		Permissions: permission.Set{
			{
				Type:  consts.Files,
				Verbs: permission.VerbSet{permission.GET: struct{}{}, permission.PATCH: struct{}{}},
			},
		},
	}
	filesAllVerbs := &permission.Permission{
		Permissions: permission.Set{
			{
				Type: consts.Files,
			},
		},
	}

	require.True(t, readOnlyForSharedOpen(filesReadOnly, false))
	require.False(t, readOnlyForSharedOpen(filesWritable, false))
	require.False(t, readOnlyForSharedOpen(filesAllVerbs, false))
	require.True(t, readOnlyForSharedOpen(filesWritable, true))
}

func TestEditor(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient(consts.Files)

	fileID := createEditorFile(t, inst, "drawing.excalidraw", "application/json", "application")

	ts := setup.GetTestServer("/editor", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("OpenLocal", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.GET("/editor/"+fileID+"/open").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.ValueEqual("type", consts.Files)
		data.ValueEqual("id", fileID)

		attrs := data.Value("attributes").Object()
		attrs.ValueEqual("file_id", fileID)
		attrs.ValueEqual("instance", inst.Domain)
		attrs.Value("protocol").String().Contains("http")
		attrs.Value("subdomain").String().NotEmpty()
		attrs.Value("public_name").String().NotEmpty()
	})

	t.Run("OpenPlainFile", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		plainID := createEditorFile(t, inst, "notes.txt", "text/plain", "text")
		obj := e.GET("/editor/"+plainID+"/open").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.file_id").String().IsEqual(plainID)
	})
}

func TestEditorOpenRecordsRecents(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	sess, err := session.New(inst, session.LongRun, "")
	require.NoError(t, err)
	_, err = permission.CreateWebappSet(inst, "drive", permission.Set{
		permission.Rule{Type: consts.Files, Verbs: permission.ALL},
	}, "1.0.0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = permission.DestroyWebapp(inst, "drive") })

	handler := echo.New()
	handler.HTTPErrorHandler = errors.ErrorHandler
	Routes(handler.Group("/editor", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("instance", inst)
			c.Set("session", sess)
			return next(c)
		}
	}))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	e := testutils.CreateTestClient(t, ts.URL)
	driveToken := inst.BuildAppToken("drive", sess.ID())

	viewedAt := func(fileID string) string {
		var row recents.FileDirActivity
		err := couchdb.GetDoc(inst, consts.FilesRecents, recents.ResourceKey(inst, fileID), &row)
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			return ""
		}
		require.NoError(t, err)
		return row.LastViewedAt
	}

	t.Run("DriveEditorsRecordAnOpen", func(t *testing.T) {
		// Drive shows drawings in its excalidraw editor and PDF files in its
		// viewer, both after opening them with this route.
		for _, file := range []struct{ name, mime, class string }{
			{"opened.excalidraw", "application/json", "application"},
			{"opened.pdf", "application/pdf", "pdf"},
		} {
			fileID := createEditorFile(t, inst, file.name, file.mime, file.class)
			e.GET("/editor/"+fileID+"/open").
				WithHeader("Authorization", "Bearer "+driveToken).
				Expect().Status(200)
			assert.NotEmpty(t, viewedAt(fileID), "opening %s is an open", file.name)
		}
	})

	t.Run("NotAPersonRecordsNothing", func(t *testing.T) {
		fileID := createEditorFile(t, inst, "oauth.excalidraw", "application/json", "application")
		_, oauthToken := setup.GetTestClient(consts.Files)
		e.GET("/editor/"+fileID+"/open").
			WithHeader("Authorization", "Bearer "+oauthToken).
			Expect().Status(200)
		assert.Empty(t, viewedAt(fileID), "an OAuth client that is not a person opens nothing")
	})
}

func createEditorFile(t *testing.T, inst *instance.Instance, name, mime, class string) string {
	filedoc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
		mime, class, time.Now(), false, false, false, nil)
	require.NoError(t, err)

	f, err := inst.VFS().CreateFile(filedoc, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return filedoc.ID()
}
