package settings_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/require"
)

func TestClientsUsage(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	testInstance := setup.GetTestInstance(&lifecycle.Options{
		Locale:      "en",
		Timezone:    "Europe/Berlin",
		Email:       "alice@example.com",
		ContextName: "test-context",
	})
	scope := consts.Settings + " " + consts.OAuthClients
	_, token := setup.GetTestClient(scope)

	svc := csettings.NewServiceMock(t)
	ts := setupRouter(t, testInstance, svc)

	flagship := oauth.Client{
		RedirectURIs: []string{"cozy://flagship"},
		ClientName:   "flagship-app",
		ClientKind:   "mobile",
		SoftwareID:   "github.com/cozy/cozy-stack/testing/flagship",
	}
	require.Nil(t, flagship.Create(testInstance, oauth.NotPending))
	storedFlagship, err := oauth.FindClient(testInstance, flagship.ClientID)
	require.NoError(t, err)
	require.NoError(t, storedFlagship.SetFlagship(testInstance))

	t.Run("WithoutLimit", func(t *testing.T) {
		testutils.WithFlag(t, testInstance, "cozy.oauthclients.max", float64(-1))

		e := testutils.CreateTestClient(t, ts.URL)
		obj := e.GET("/settings/clients-usage").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.clients-usage")

		attrs := data.Value("attributes").Object()
		attrs.NotContainsKey("limit")
		attrs.HasValue("count", 1)
		attrs.HasValue("limitReached", false)
		attrs.HasValue("limitExceeded", false)
	})

	t.Run("WithLimitNotReached", func(t *testing.T) {
		testutils.WithFlag(t, testInstance, "cozy.oauthclients.max", float64(2))

		e := testutils.CreateTestClient(t, ts.URL)
		obj := e.GET("/settings/clients-usage").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.clients-usage")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("limit", 2)
		attrs.HasValue("count", 1)
		attrs.HasValue("limitReached", false)
		attrs.HasValue("limitExceeded", false)
	})

	t.Run("WithLimitReached", func(t *testing.T) {
		testutils.WithFlag(t, testInstance, "cozy.oauthclients.max", float64(1))

		e := testutils.CreateTestClient(t, ts.URL)
		obj := e.GET("/settings/clients-usage").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.clients-usage")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("limit", 1)
		attrs.HasValue("count", 1)
		attrs.HasValue("limitReached", true)
		attrs.HasValue("limitExceeded", false)
	})

	t.Run("WithLimitExceeded", func(t *testing.T) {
		testutils.WithFlag(t, testInstance, "cozy.oauthclients.max", float64(0))

		e := testutils.CreateTestClient(t, ts.URL)
		obj := e.GET("/settings/clients-usage").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.clients-usage")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("limit", 0)
		attrs.HasValue("count", 1)
		attrs.HasValue("limitReached", true)
		attrs.HasValue("limitExceeded", true)
	})
}
