package oauth_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/tests/testutils"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/cozy/cozy-stack/model/notification/center"
	_ "github.com/cozy/cozy-stack/worker/mails"
)

var c = &oauth.Client{
	CouchID: "my-client-id",
}

func TestGetLinkedAppSlug(t *testing.T) {
	assert.Equal(t, "mail", oauth.GetLinkedAppSlug("registry://mail"))
	assert.Equal(t, "mail", oauth.GetLinkedAppSlug("registry://mail/stable"))
	assert.Equal(t, "mail", oauth.GetLinkedAppSlug("registry://mail/dev/latest"))
	assert.Empty(t, oauth.GetLinkedAppSlug("github.com/cozy/mail"))
	assert.Empty(t, oauth.GetLinkedAppSlug("registry://"))
}

func TestClient(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	testInstance := setup.GetTestInstance()

	t.Run("CreateJWT", func(t *testing.T) {
		tokenString, err := c.CreateJWT(testInstance, "test", "foo:read")
		assert.NoError(t, err)

		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			_, ok := token.Method.(*jwt.SigningMethodHMAC)
			assert.True(t, ok, "The signing method should be HMAC")
			return testInstance.OAuthSecret, nil
		})
		assert.NoError(t, err)
		assert.True(t, token.Valid)

		claims, ok := token.Claims.(jwt.MapClaims)
		assert.True(t, ok, "Claims can be parsed as standard claims")
		assert.Equal(t, []interface{}{"test"}, claims["aud"])
		assert.Equal(t, testInstance.Domain, claims["iss"])
		assert.Equal(t, "my-client-id", claims["sub"])
		assert.Equal(t, "foo:read", claims["scope"])
	})

	t.Run("ParseJWT", func(t *testing.T) {
		tokenString, err := c.CreateJWT(testInstance, "refresh", "foo:read")
		assert.NoError(t, err)

		claims, ok := c.ValidToken(testInstance, consts.RefreshTokenAudience, tokenString)
		assert.True(t, ok, "The token must be valid")
		assert.Equal(t, jwt.ClaimStrings{"refresh"}, claims.Audience)
		assert.Equal(t, testInstance.Domain, claims.Issuer)
		assert.Equal(t, "my-client-id", claims.Subject)
		assert.Equal(t, "foo:read", claims.Scope)
	})

	t.Run("ParseJWTInvalidAudience", func(t *testing.T) {
		tokenString, err := c.CreateJWT(testInstance, "access", "foo:read")
		assert.NoError(t, err)
		_, ok := c.ValidToken(testInstance, consts.RefreshTokenAudience, tokenString)
		assert.False(t, ok, "The token should be invalid")
	})

	t.Run("CreateClient", func(t *testing.T) {
		client := &oauth.Client{
			ClientName:   "foo",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",

			NotificationPlatform:    "android",
			NotificationDeviceToken: "foobar",
		}
		assert.Nil(t, client.Create(testInstance))

		client2 := &oauth.Client{
			ClientName:   "foo",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",

			NotificationPlatform:    "ios",
			NotificationDeviceToken: "foobar",
		}
		assert.Nil(t, client2.Create(testInstance))

		client3 := &oauth.Client{
			ClientName:   "foo",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		assert.Nil(t, client3.Create(testInstance))

		client4 := &oauth.Client{
			ClientName:   "foo-2",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		assert.Nil(t, client4.Create(testInstance))

		assert.Equal(t, "foo", client.ClientName)
		assert.Equal(t, "foo-2", client2.ClientName)
		assert.Equal(t, "foo-3", client3.ClientName)
		assert.Equal(t, "foo-2-2", client4.ClientName)
	})

	t.Run("CreateClientWithNotifications", func(t *testing.T) {
		goodClient := &oauth.Client{
			ClientName:   "client-5",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		if !assert.Nil(t, goodClient.Create(testInstance)) {
			return
		}

		{
			var err error
			goodClient, err = oauth.FindClient(testInstance, goodClient.ClientID)
			require.NoError(t, err)
		}

		{
			client := goodClient.Clone().(*oauth.Client)
			client.NotificationPlatform = "android"
			assert.Nil(t, client.Update(testInstance, goodClient))
		}

		{
			client := goodClient.Clone().(*oauth.Client)
			client.NotificationPlatform = "unknown"
			assert.NotNil(t, client.Update(testInstance, goodClient))
		}
	})

	t.Run("CreateClientWithClientsLimit", func(t *testing.T) {
		var pending, notPending, notificationWithoutPremium, notificationWithPremium *oauth.Client
		t.Cleanup(func() {
			// Delete created clients
			pending, err := oauth.FindClient(testInstance, pending.ClientID)
			require.NoError(t, err)
			require.Nil(t, pending.Delete(testInstance))

			notPending, err := oauth.FindClient(testInstance, notPending.ClientID)
			require.NoError(t, err)
			require.Nil(t, notPending.Delete(testInstance))

			notificationWithoutPremium, err := oauth.FindClient(testInstance, notificationWithoutPremium.ClientID)
			require.NoError(t, err)
			require.Nil(t, notificationWithoutPremium.Delete(testInstance))

			notificationWithPremium, err := oauth.FindClient(testInstance, notificationWithPremium.ClientID)
			require.NoError(t, err)
			require.Nil(t, notificationWithPremium.Delete(testInstance))
		})

		pending = &oauth.Client{
			ClientName:   "pending",
			ClientKind:   "mobile",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, pending.Create(testInstance))
		assertClientsLimitAlertMailWasNotSent(t, testInstance)

		notPending = &oauth.Client{
			ClientName:   "notPending",
			ClientKind:   "mobile",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, notPending.Create(testInstance, oauth.NotPending))
		assertClientsLimitAlertMailWasNotSent(t, testInstance)

		testutils.WithFlag(t, testInstance, "cozy.oauthclients.max", float64(1))

		notificationWithoutPremium = &oauth.Client{
			ClientName:   "notificationWithoutPremium",
			ClientKind:   "mobile",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, notificationWithoutPremium.Create(testInstance, oauth.NotPending))
		premiumLink := assertClientsLimitAlertMailWasSent(t, testInstance, "notificationWithoutPremium", 1)
		assert.Empty(t, premiumLink)

		testutils.WithManager(t, testInstance, testutils.ManagerConfig{URL: "http://manager.example.org", WithPremiumLinks: true})

		notificationWithPremium = &oauth.Client{
			ClientName:   "notificationWithPremium",
			ClientKind:   "mobile",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, notificationWithPremium.Create(testInstance, oauth.NotPending))
		premiumLink = assertClientsLimitAlertMailWasSent(t, testInstance, "notificationWithPremium", 1)
		assert.NotEmpty(t, premiumLink)
	})

	t.Run("GetConnectedUserClients", func(t *testing.T) {
		browser := &oauth.Client{
			ClientName:   "browser",
			ClientKind:   "browser",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, browser.Create(testInstance, oauth.NotPending))

		desktop := &oauth.Client{
			ClientName:   "desktop",
			ClientKind:   "desktop",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, desktop.Create(testInstance, oauth.NotPending))

		mobile := &oauth.Client{
			ClientName:   "mobile",
			ClientKind:   "mobile",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, mobile.Create(testInstance, oauth.NotPending))

		pending := &oauth.Client{
			ClientName:   "pending",
			ClientKind:   "desktop",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, pending.Create(testInstance))

		sharing := &oauth.Client{
			ClientName:   "sharing",
			ClientKind:   "sharing",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, sharing.Create(testInstance, oauth.NotPending))

		incomplete := &oauth.Client{
			ClientName:   "incomplete",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "bar",
		}
		require.Nil(t, incomplete.Create(testInstance, oauth.NotPending))

		clients, _, err := oauth.GetConnectedUserClients(testInstance, 100, "")
		require.NoError(t, err)

		assert.Len(t, clients, 3)
		assert.Equal(t, clients[0].ClientName, "browser")
		assert.Equal(t, clients[1].ClientName, "desktop")
		assert.Equal(t, clients[2].ClientName, "mobile")
	})

	t.Run("ParseJWTInvalidIssuer", func(t *testing.T) {
		other := &instance.Instance{
			OAuthSecret: testInstance.OAuthSecret,
			Domain:      "other.example.com",
		}
		tokenString, err := c.CreateJWT(other, "refresh", "foo:read")
		assert.NoError(t, err)
		_, ok := c.ValidToken(testInstance, consts.RefreshTokenAudience, tokenString)
		assert.False(t, ok, "The token should be invalid")
	})

	t.Run("ParseJWTValidWithOldDomain", func(t *testing.T) {
		// Simulate a migrated instance where the domain changed but OldDomain is set
		originalDomain := testInstance.Domain

		// Create a refresh token with the current domain as issuer
		tokenString, err := c.CreateJWT(testInstance, "refresh", "foo:read")
		assert.NoError(t, err)

		// Simulate migration: change Domain and set OldDomain to the original
		testInstance.Domain = "new.example.com"
		testInstance.OldDomain = originalDomain

		// The token should still be valid because OldDomain matches the issuer
		claims, ok := c.ValidToken(testInstance, consts.RefreshTokenAudience, tokenString)
		assert.True(t, ok, "The token should be valid when OldDomain matches the issuer")
		assert.Equal(t, originalDomain, claims.Issuer)
		assert.Equal(t, "foo:read", claims.Scope)

		// Restore the instance
		testInstance.Domain = originalDomain
		testInstance.OldDomain = ""
	})

	t.Run("ParseJWTInvalidSubject", func(t *testing.T) {
		other := &oauth.Client{
			CouchID: "my-other-client",
		}
		tokenString, err := other.CreateJWT(testInstance, "refresh", "foo:read")
		assert.NoError(t, err)
		_, ok := c.ValidToken(testInstance, consts.RefreshTokenAudience, tokenString)
		assert.False(t, ok, "The token should be invalid")
	})

	t.Run("ParseGoodSoftwareID", func(t *testing.T) {
		goodClient := &oauth.Client{
			ClientName:   "client-5",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "registry://drive",
		}
		err := goodClient.CheckSoftwareID(testInstance)
		assert.Nil(t, err)
	})

	t.Run("ParseHttpSoftwareID", func(t *testing.T) {
		goodClient := &oauth.Client{
			ClientName:   "client-5",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "https://github.com/cozy-labs/cozy-desktop",
		}
		err := goodClient.CheckSoftwareID(testInstance)
		assert.Nil(t, err)
	})

	t.Run("SortCLientsByCreatedAtDesc", func(t *testing.T) {
		t0 := time.Now().Add(-1 * time.Minute)
		t1 := t0.Add(10 * time.Second)
		t2 := t1.Add(10 * time.Second)
		clients := []*oauth.Client{
			{CouchID: "a", Metadata: &metadata.CozyMetadata{CreatedAt: t2}},
			{CouchID: "d"},
			{CouchID: "c", Metadata: &metadata.CozyMetadata{CreatedAt: t0}},
			{CouchID: "e"},
			{CouchID: "b", Metadata: &metadata.CozyMetadata{CreatedAt: t1}},
		}
		oauth.SortClientsByCreatedAtDesc(clients)
		require.Len(t, clients, 5)
		assert.Equal(t, "a", clients[0].CouchID)
		assert.Equal(t, "b", clients[1].CouchID)
		assert.Equal(t, "c", clients[2].CouchID)
		assert.Equal(t, "d", clients[3].CouchID)
		assert.Equal(t, "e", clients[4].CouchID)
	})

	t.Run("CheckOAuthClientsLimitReached", func(t *testing.T) {
		require.NoError(t, couchdb.ResetDB(testInstance, consts.OAuthClients))

		// Create the OAuth client for the flagship app
		flagship := oauth.Client{
			RedirectURIs: []string{"cozy://flagship"},
			ClientName:   "flagship-app",
			ClientKind:   "mobile",
			SoftwareID:   "github.com/cozy/cozy-stack/testing/flagship",
			Flagship:     true,
		}
		require.Nil(t, flagship.Create(testInstance, oauth.NotPending))

		clients, _, err := oauth.GetConnectedUserClients(testInstance, 100, "")
		require.NoError(t, err)
		require.Equal(t, len(clients), 1)

		var reached, exceeded bool

		reached, exceeded = oauth.CheckOAuthClientsLimitReached(testInstance, 0)
		require.True(t, reached)
		require.True(t, exceeded)

		reached, exceeded = oauth.CheckOAuthClientsLimitReached(testInstance, 1)
		require.True(t, reached)
		require.False(t, exceeded)

		reached, exceeded = oauth.CheckOAuthClientsLimitReached(testInstance, 2)
		require.False(t, reached)
		require.False(t, exceeded)

		reached, exceeded = oauth.CheckOAuthClientsLimitReached(testInstance, -1)
		require.False(t, reached)
		require.False(t, exceeded)
	})

	t.Run("checkPlayIntegrityAttestation", func(t *testing.T) {
		config := config.GetConfig()
		config.Flagship.PlayIntegrityDecryptionKeys = []string{"bVcBAv0eO64NKIvDoRHpnTOZVxAkhMuFwRHrTEMr23U="}
		config.Flagship.PlayIntegrityVerificationKeys = []string{"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAElTF2uARN7oxfoDWyERYMe6QutI2NqS+CAtVmsPDIRjBBxF96fYojFVXRRsMb86PjkE21Ol+sO1YuspY+YuDRMw=="}

		req := oauth.AttestationRequest{
			Platform:    "android",
			Issuer:      "playintegrity",
			Challenge:   "testtesttesttesttesttesttest",
			Attestation: "eyJhbGciOiJBMjU2S1ciLCJlbmMiOiJBMjU2R0NNIn0.MZIbC3rTckzCtg4rdAatQObifb3hkgJSq7-_XYTLItiCjkOyEjORlQ.-Z-6QJyEx4Bf4fNp.4vFq2XQvgQESouc5fF-oSixpYwWL2FBDzHfw1ay8nHmCXAgYfJ1yRPJm09dvJWJ5Iez4-HvfRWkwstZ4gtGYr4SX42h7L0vWkcv8yJ-12X9kUAFM_7ylpBLWiDEHnd0SeqpSeiAut_XXD81A_SncaenicMzDi0QKqeD6bdAkY67h46hnuyektYU4AsK9nVRPStaEfNiREJ017PuRVP3JQZVk4vAvg0jMfdY3BnaQ3AiEMb6uredrgP29gIIs0mGwcvc7ONyVRZ4_gSDSmfqKBjG-7HuC_rmC9CL2cUoz_JRxY0njvJi7isyfoTVZMyI4TKbUQckTKvv1Ysv11FxlTVsQqmOkVKtHOemS-G9ji23rq-LcGHG1DyriNqd3aFjMD6s1p5tFpxg7Eyc3pEm4f1Ig4S-sOC6BsTjqM_cNyqCuNbfwtQSE1pnh7yI7pcsfLPRisoODng0wTYXAqA4mvATf60eKSrPGb6vD47owlV-CbxLkG3PpVhjIpLIGknFSJnkzeIdgTR5XWUsQKVJ6ppW4mq8tO_C4KNHNISKimUhmFekG1w1rZ_suAvaC5Oz6NKn4iVMXpNm3N8nuBCkwbenN_A7334rSMHS12Ye1QRiH54VuUksUmzeUiFxaubkEJGVHwxYDN_lwQZ7bzSZbMfW46_-rK98SC3JNkif4Ucdl52fWY8Mpaf41PYGv6H7QAnY94wkAZGJPmaCzicDs5UbAiCI.fqVFSJEaY7GiqCga4-CMuw",
		}
		require.NoError(t, oauth.CheckPlayIntegrityAttestationForTestingPurpose(req))
	})
}

func assertClientsLimitAlertMailWasNotSent(t *testing.T, instance *instance.Instance) {
	var jobs []job.Job
	couchReq := &couchdb.FindRequest{
		UseIndex: "by-worker-and-state",
		Selector: mango.And(
			mango.Equal("worker", "sendmail"),
			mango.Exists("state"),
		),
		Sort: mango.SortBy{
			mango.SortByField{Field: "worker", Direction: "desc"},
		},
		Limit: 1,
	}
	err := couchdb.FindDocs(instance, consts.Jobs, couchReq, &jobs)
	assert.NoError(t, err)

	// Mail sent for the device connection
	assert.Len(t, jobs, 0)
}

func assertClientsLimitAlertMailWasSent(t *testing.T, instance *instance.Instance, clientName string, clientsLimit int) string {
	var jobs []job.Job
	couchReq := &couchdb.FindRequest{
		UseIndex: "by-worker-and-state",
		Selector: mango.And(
			mango.Equal("worker", "sendmail"),
			mango.Exists("state"),
		),
		Sort: mango.SortBy{
			mango.SortByField{Field: "worker", Direction: "desc"},
		},
		Limit: 1,
	}
	err := couchdb.FindDocs(instance, consts.Jobs, couchReq, &jobs)
	assert.NoError(t, err)
	assert.Len(t, jobs, 1)

	var msg map[string]interface{}
	err = json.Unmarshal(jobs[0].Message, &msg)
	assert.NoError(t, err)

	assert.Equal(t, msg["mode"], "noreply")
	assert.Equal(t, msg["template_name"], "notifications_oauthclients")

	values := msg["template_values"].(map[string]interface{})
	assert.Equal(t, values["ClientName"], clientName)
	assert.Equal(t, values["ClientsLimit"], float64(clientsLimit))

	return values["OffersLink"].(string)
}
