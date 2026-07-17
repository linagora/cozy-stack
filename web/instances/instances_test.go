package instances

import (
	"net/url"
	"testing"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
)

func TestInstances(t *testing.T) {
	config.UseTestFile(t)

	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())

	config.GetConfig().Fs.URL = &url.URL{
		Scheme: "file",
		Host:   "localhost",
		Path:   t.TempDir(),
	}

	_, token := setup.GetTestAdminClient()
	ts := setup.GetTestServer("/instances", Routes, func(r *echo.Echo) *echo.Echo {
		secure := middlewares.Secure(&middlewares.SecureConfig{
			CSPDefaultSrc:     []middlewares.CSPSource{middlewares.CSPSrcSelf},
			CSPFrameAncestors: []middlewares.CSPSource{middlewares.CSPSrcNone},
		})
		r.Use(secure)
		return r
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("Create", func(t *testing.T) {
		t.Run("WithoutOrganizationDomain", func(t *testing.T) {
			domain := "alice.cozy.localhost"
			t.Cleanup(func() { _ = lifecycle.Destroy(domain) })

			e := testutils.CreateTestClient(t, ts.URL)

			// Create instance with feature sets
			e.POST("/instances").
				WithQuery("DiskQuota", 5000000000).
				WithQuery("Domain", domain).
				WithQuery("Email", "alice@example.com").
				WithQuery("Locale", "en").
				WithQuery("PublicName", "alice").
				WithQuery("Settings", "partner:cozy,context:cozy_beta,tos:1.0.0").
				WithQuery("SwiftLayout", "-1").
				WithQuery("TOSSigned", "1.0.0").
				WithQuery("UUID", "60bac7e8-abd9-11ee-8201-9cb6d0907fa3").
				WithQuery("feature_sets", "71df3022-abd9-11ee-b79b-9cb6d0907fa3,790789f8-abd9-11ee-ae09-9cb6d0907fa3").
				WithQuery("sponsorships", "").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().Path("$.data.attributes.feature_sets").
				Array().
				HasValue(0, "71df3022-abd9-11ee-b79b-9cb6d0907fa3").
				HasValue(1, "790789f8-abd9-11ee-ae09-9cb6d0907fa3")
		})

		t.Run("WithOrganizationDomainAndOrgID", func(t *testing.T) {
			domain := "alice.cozy.localhost"
			t.Cleanup(func() { _ = lifecycle.Destroy(domain) })

			e := testutils.CreateTestClient(t, ts.URL)

			// Create instance with org_domain and org_id
			attrs := e.POST("/instances").
				WithQuery("DiskQuota", 5000000000).
				WithQuery("Domain", domain).
				WithQuery("OrgDomain", "example.com").
				WithQuery("OrgID", "org-123-abc").
				WithQuery("Email", "alice@example.com").
				WithQuery("Locale", "en").
				WithQuery("PublicName", "alice").
				WithQuery("Settings", "partner:cozy,context:cozy_beta,tos:1.0.0").
				WithQuery("SwiftLayout", "-1").
				WithQuery("TOSSigned", "1.0.0").
				WithQuery("UUID", "60bac7e8-abd9-11ee-8201-9cb6d0907fa3").
				WithQuery("feature_sets", "71df3022-abd9-11ee-b79b-9cb6d0907fa3,790789f8-abd9-11ee-ae09-9cb6d0907fa3").
				WithQuery("sponsorships", "").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().Path("$.data.attributes").Object()

			attrs.HasValue("org_domain", "example.com")
			attrs.HasValue("org_id", "org-123-abc")
			attrs.HasValue("feature_sets", []string{"71df3022-abd9-11ee-b79b-9cb6d0907fa3", "790789f8-abd9-11ee-ae09-9cb6d0907fa3"})
		})
	})

	t.Run("MigrateStorage", func(t *testing.T) {
		domain := "migrate-storage.cozy.localhost"
		t.Cleanup(func() { _ = lifecycle.Destroy(domain) })

		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/instances").
			WithQuery("Domain", domain).
			WithQuery("Locale", "en").
			WithQuery("SwiftLayout", "-1").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		// An unsupported target scheme is rejected by the Migrate guard
		// before touching any storage backend, so this exercises the route
		// and error propagation without requiring a live S3/Swift target.
		e.POST("/instances/"+domain+"/migrate-storage").
			WithQuery("to", "not-a-scheme").
			WithQuery("dry_run", "true").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(500)
	})
}
