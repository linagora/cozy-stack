package settings_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cozy/cozy-stack/model/cloudery"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/session"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gavv/httpexpect/v2"
)

func TestExternalTies(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	testInstance := setup.GetTestInstance(&lifecycle.Options{
		Locale:      "en",
		Timezone:    "Europe/Berlin",
		Email:       "alice@example.com",
		ContextName: "test-context",
	})
	sessCookie := session.CookieName(testInstance)

	svc := csettings.NewServiceMock(t)
	ts := setupRouter(t, testInstance, svc)

	t.Run("GetExternalTies", func(t *testing.T) {
		t.Run("WithBlockingSubscription", func(t *testing.T) {
			blockingSubscription := cloudery.BlockingSubscription{Vendor: "ios"}

			svc.On("GetExternalTies", testInstance).Return(&csettings.ExternalTies{
				HasBlockingSubscription: true,
				BlockingSubscription:    &blockingSubscription,
			}, nil).Once()

			e := testutils.CreateTestClient(t, ts.URL)
			obj := e.GET("/settings/external-ties").
				WithCookie(sessCookie, "connected").
				WithHeader("Accept", "application/vnd.api+json").
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			data.HasValue("type", "io.cozy.settings")
			data.HasValue("id", "io.cozy.settings.external-ties")
			data.Value("links").Object().HasValue("self", "/settings/external-ties")

			attrs := data.Value("attributes").Object()
			attrs.HasValue("has_blocking_subscription", true)
			attrs.Value("blocking_subscription").Object().HasValue("vendor", "ios")
		})

		t.Run("WithoutBlockingSubscription", func(t *testing.T) {
			svc.On("GetExternalTies", testInstance).Return(&csettings.ExternalTies{}, nil).Once()

			e := testutils.CreateTestClient(t, ts.URL)
			obj := e.GET("/settings/external-ties").
				WithCookie(sessCookie, "connected").
				WithHeader("Accept", "application/vnd.api+json").
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			data.HasValue("type", "io.cozy.settings")
			data.HasValue("id", "io.cozy.settings.external-ties")

			attrs := data.Value("attributes").Object()
			attrs.HasValue("has_blocking_subscription", false)
			attrs.NotContainsValue("blocking_subscription")
		})

		t.Run("WithClouderyError", func(t *testing.T) {
			svc.On("GetExternalTies", testInstance).Return(nil, errors.New("unauthorized")).Once()

			e := testutils.CreateTestClient(t, ts.URL)
			e.GET("/settings/external-ties").
				WithCookie(sessCookie, "connected").
				WithHeader("Accept", "application/vnd.api+json").
				Expect().Status(500).
				Body().Contains("unauthorized")
		})
	})

	t.Run("RedirectToPremium", func(t *testing.T) {
		t.Run("WithManager", func(t *testing.T) {
			testutils.WithManager(t, testInstance, testutils.ManagerConfig{URL: "http://manager.localhost", WithPremiumLinks: true})

			// HTTP client that does not follow redirections so we can test them
			noRedirectClient := &http.Client{
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse
				},
				Jar: httpexpect.NewCookieJar(), // XXX: used in httpexpect default client config
			}

			e := testutils.CreateTestClient(t, ts.URL)
			e.GET("/settings/premium").
				WithClient(noRedirectClient).
				WithCookie(sessCookie, "connected").
				Expect().Status(301).
				Header("Location").IsEqual(fmt.Sprintf("http://manager.localhost/cozy/instances/%s/premium", testInstance.UUID))
		})

		t.Run("WithoutManager", func(t *testing.T) {
			// XXX: This will update the instance and its context. Tests coming
			// after this one won't have a manager unless one is added using
			// testutils.WithManager.
			testutils.DisableManager(testInstance, true)

			e := testutils.CreateTestClient(t, ts.URL)
			e.GET("/settings/premium").
				WithCookie(sessCookie, "connected").
				Expect().Status(404).
				Body().Contains("This instance does not have a premium subscription manager.")
		})
	})
}
