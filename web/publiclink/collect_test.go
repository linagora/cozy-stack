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
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gofrs/uuid/v5"
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

	inst := &instance.Instance{Domain: "example.test", Prefix: "!invalid", FeatureFlags: map[string]interface{}{modelpubliclink.FlagEnabled: true}}
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

func TestRecordContextFlag(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	id := uuid.Must(uuid.NewV7()).String()
	inst := &instance.Instance{DocID: id, Domain: "flag-" + id, ContextName: "flag-" + id}
	perm := &permission.Permission{PID: "link", Type: permission.TypeShareByLink, Permissions: permission.Set{{Type: consts.Files}}}
	require.NoError(t, couchdb.CreateDB(inst, consts.PublicLinkEvents))
	t.Cleanup(func() { _ = couchdb.DeleteDB(inst, consts.PublicLinkEvents) })
	e := echo.New()
	e.GET("/:action", func(c echo.Context) error {
		Record(c, inst, perm, modelpubliclink.Action(c.Param("action")))
		return c.String(http.StatusOK, "original response")
	})
	server := httptest.NewServer(e)
	defer server.Close()
	request := func(action modelpubliclink.Action) {
		t.Helper()
		resp, err := http.Get(server.URL + "/" + string(action))
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "original response", string(body))
	}
	events := func() []modelpubliclink.Event {
		t.Helper()
		var result []modelpubliclink.Event
		require.NoError(t, couchdb.FindDocsUnoptimized(inst, consts.PublicLinkEvents, &couchdb.FindRequest{Selector: mango.Equal("link_id", "link")}, &result))
		return result
	}
	request(modelpubliclink.Created)
	require.Empty(t, events(), "disabled by default")
	flags := &couchdb.JSONDoc{Type: consts.Settings, M: map[string]interface{}{
		"_id":                       consts.ContextFlagsSettingsID + "." + inst.ContextName,
		modelpubliclink.FlagEnabled: []interface{}{map[string]interface{}{"ratio": 1.0, "value": true}},
	}}
	require.NoError(t, couchdb.CreateNamedDocWithDB(prefixer.GlobalPrefixer, flags))
	t.Cleanup(func() { _ = couchdb.DeleteDoc(prefixer.GlobalPrefixer, flags) })
	for _, action := range []modelpubliclink.Action{modelpubliclink.Created, modelpubliclink.Updated, modelpubliclink.Opened, modelpubliclink.Downloaded} {
		request(action)
	}
	counts := map[modelpubliclink.Action]int{}
	for _, event := range events() {
		counts[event.Action]++
	}
	require.Equal(t, map[modelpubliclink.Action]int{modelpubliclink.Created: 1, modelpubliclink.Updated: 1, modelpubliclink.Opened: 1, modelpubliclink.Downloaded: 1}, counts)
	for _, value := range []interface{}{false, "true", nil} {
		inst.FeatureFlags = map[string]interface{}{modelpubliclink.FlagEnabled: value}
		request(modelpubliclink.Opened)
		require.Len(t, events(), 4, "instance value %#v overrides context", value)
	}
}
