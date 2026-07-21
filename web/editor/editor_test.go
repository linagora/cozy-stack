package editor

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
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

func createEditorFile(t *testing.T, inst *instance.Instance, name, mime, class string) string {
	filedoc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
		mime, class, time.Now(), false, false, false, nil)
	require.NoError(t, err)

	f, err := inst.VFS().CreateFile(filedoc, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return filedoc.ID()
}
