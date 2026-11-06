package publiclink

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	modelpubliclink "github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
)

func TestRecordStorageFailureDoesNotChangeResponse(t *testing.T) {
	require.NoError(t, config.LoadTestFile())
	testutils.NeedCouchdb(t)
	oldHooks := logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks))
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(oldHooks) })
	hook := logtest.NewGlobal()

	inst := &instance.Instance{Domain: "example.test", Prefix: "!invalid"}
	perm := &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{{Type: consts.Files}}}
	e := echo.New()
	e.GET("/", func(c echo.Context) error {
		Record(c, inst, perm, modelpubliclink.Opened)
		return c.String(http.StatusOK, "original response")
	})
	server := httptest.NewServer(e)
	defer server.Close()
	resp, err := http.Get(server.URL + "/?sharecode=secret-sharecode")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "original response", string(body))
	var found bool
	for _, entry := range hook.AllEntries() {
		if entry.Data["nspace"] == "publiclink" {
			found = true
			require.Contains(t, entry.Message, "CouchDB status 400")
			require.NotContains(t, entry.Message, "secret-sharecode")
		}
	}
	require.True(t, found, "storage failure should be logged")
}
