package instances

import (
	"net/url"
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/opensearch"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestSearchRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	t.Setenv("COZY_SEARCH_OPENSEARCH_URL", "")
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, token := setup.GetTestAdminClient()
	ts := setup.GetTestServer("/instances", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)
	e := testutils.CreateTestClient(t, ts.URL)
	auth := "Bearer " + token
	base := "/instances/" + inst.Domain + "/search/"

	t.Run("search is not configured", func(t *testing.T) {
		e.POST(base+"enable").WithHeader("Authorization", auth).Expect().Status(503)
		e.POST(base+"reset").WithHeader("Authorization", auth).Expect().Status(503)
		e.GET(base+"status").WithHeader("Authorization", auth).Expect().Status(503)
	})

	t.Run("unknown instance", func(t *testing.T) {
		e.GET("/instances/unknown.example.net/search/status").WithHeader("Authorization", auth).Expect().Status(404)
	})

	t.Run("status", func(t *testing.T) {
		// The status reads CouchDB only: this OpenSearch is never called
		u, err := url.Parse("http://127.0.0.1:1")
		require.NoError(t, err)
		config.GetConfig().Search = opensearch.NewClient(opensearch.Options{URL: u})
		t.Cleanup(func() { config.GetConfig().Search = nil })
		e.GET(base+"status").WithHeader("Authorization", auth).
			Expect().Status(200).
			JSON().Object().IsEqual(map[string]any{"trigger": false, "last_seq": ""})
	})
}
