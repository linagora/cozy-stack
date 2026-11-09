package files

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectoryPathAuthorization(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, writable := setup.GetTestClient(consts.Files)
	_, readOnly := setup.GetTestClient(consts.Files + ":GET")
	_, scoped := setup.GetTestClient(consts.Files + ":ALL:some-folder")
	ts := setup.GetTestServer("/files", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)
	e := testutils.CreateTestClient(t, ts.URL)
	for _, token := range []string{"", readOnly, scoped} {
		status := http.StatusForbidden
		if token == "" {
			status = http.StatusUnauthorized
		}
		for _, recursive := range []bool{false, true} {
			e.POST("/files/").WithQuery("Type", "directory").WithQuery("Path", "/blocked/child").
				WithQuery("Recursive", recursive).WithHeader("Authorization", "Bearer "+token).
				Expect().Status(status)
		}
	}
	exists, err := vfs.DirExists(inst.VFS(), "/blocked")
	require.NoError(t, err)
	assert.False(t, exists)
	e.POST("/files/").WithQuery("Type", "directory").WithQuery("Path", "/allowed/child").
		WithQuery("Recursive", true).WithHeader("Authorization", "Bearer "+writable).
		Expect().Status(http.StatusCreated)
	e.POST("/files/").WithQuery("Type", "directory").WithQuery("Path", "/allowed/child").
		WithQuery("Recursive", true).Expect().Status(http.StatusUnauthorized)
}

func TestSharedDirectoryRejectsAbsolutePath(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/?Path=/outside&Recursive=true", nil),
		httptest.NewRecorder())
	middlewares.SetInstance(c, &instance.Instance{})
	middlewares.ForcePermission(c, &permission.Permission{Permissions: permission.Set{{Type: consts.Files}}})
	// A nil VFS also ensures the request is rejected before touching the owner's files.
	_, err := createDirHandler(c, nil, &sharing.Sharing{})
	require.Error(t, err)
}
