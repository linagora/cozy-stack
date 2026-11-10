package auth

import (
	"net/url"
	"testing"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateTokenExchangeScope(t *testing.T) {
	assert.Error(t, validateTokenExchangeScope(""))
	assert.Error(t, validateTokenExchangeScope("io.cozy.unknown"))
	assert.Error(t, validateTokenExchangeScope("io.cozy.files\tio.cozy.contacts\tio.cozy.contacts.groups\tio.cozy.apps\tio.cozy.sharings"))
	assert.NoError(t, validateTokenExchangeScope("io.cozy.files"))
	assert.NoError(t, validateTokenExchangeScope("io.cozy.files io.cozy.contacts io.cozy.contacts.groups io.cozy.apps io.cozy.sharings"))
}

func TestTokenExchangeRequestExchangeType(t *testing.T) {
	exchangeType, err := tokenExchangeRequestExchangeType("")
	assert.NoError(t, err)
	assert.Equal(t, tokenExchangeTypeAdmin, exchangeType)

	exchangeType, err = tokenExchangeRequestExchangeType("admin")
	assert.NoError(t, err)
	assert.Equal(t, tokenExchangeTypeAdmin, exchangeType)

	exchangeType, err = tokenExchangeRequestExchangeType(" app ")
	assert.NoError(t, err)
	assert.Equal(t, tokenExchangeTypeApp, exchangeType)

	_, err = tokenExchangeRequestExchangeType("user")
	assert.EqualError(t, err, `exchange_type "user" is not allowed`)
}

func TestTokenExchangeAssertManifestTrusted(t *testing.T) {
	mk := func(source string) *app.WebappManifest {
		m := &app.WebappManifest{}
		m.SetSlug("mail")
		if source != "" {
			u, err := url.Parse(source)
			require.NoError(t, err)
			m.SetSource(u)
		}
		return m
	}

	assert.NoError(t, tokenExchangeAssertManifestTrusted(mk("registry://mail"), "mail"))
	assert.NoError(t, tokenExchangeAssertManifestTrusted(mk("registry://mail/stable"), "mail"))

	err := tokenExchangeAssertManifestTrusted(mk(""), "mail")
	assert.EqualError(t, err, "code=400, message=mail is not a registry application")

	err = tokenExchangeAssertManifestTrusted(mk("git://example.com/mail"), "mail")
	assert.EqualError(t, err, "code=400, message=mail is not a registry application")

	err = tokenExchangeAssertManifestTrusted(mk("registry://contacts"), "mail")
	assert.EqualError(t, err, "code=400, message=mail is not the installed application")

	err = tokenExchangeAssertManifestTrusted(mk("registry://mailbox/stable"), "mail")
	assert.EqualError(t, err, "code=400, message=mail is not the installed application")
}

func TestTokenExchangeMatchingAppConfig(t *testing.T) {
	appConfigs := map[string]config.OIDCAppTokenExchangeAppConfig{
		"twake-mail-web": {SoftwareID: "registry://mail"},
		"n8n":            {SoftwareID: "registry://n8n"},
	}
	match := func(claims jwt.MapClaims) string {
		appConfig, ok := tokenExchangeMatchingAppConfig(claims, appConfigs)
		if !ok {
			return ""
		}
		return appConfig.SoftwareID
	}

	// The client the token was issued to wins, whatever the order of aud
	assert.Equal(t, "registry://n8n", match(jwt.MapClaims{
		"aud": []string{"twake-mail-web", "n8n"}, "azp": "n8n",
	}))
	assert.Equal(t, "registry://mail", match(jwt.MapClaims{
		"aud": []string{"n8n", "twake-mail-web"}, "azp": "twake-mail-web",
	}))
	assert.Equal(t, "registry://mail", match(jwt.MapClaims{"aud": "twake-mail-web"}))
	assert.Equal(t, "registry://n8n", match(jwt.MapClaims{"aud": []string{"n8n"}}))

	// Several audiences and no azp: nothing says which client asked
	assert.Empty(t, match(jwt.MapClaims{"aud": []string{"twake-mail-web", "n8n"}}))
	// azp must be one of the audiences
	assert.Empty(t, match(jwt.MapClaims{"aud": []string{"twake-mail-web"}, "azp": "n8n"}))
	// A mapped audience does not stand in for an unmapped azp
	assert.Empty(t, match(jwt.MapClaims{
		"aud": []string{"other", "twake-mail-web"}, "azp": "other",
	}))
	assert.Empty(t, match(jwt.MapClaims{"aud": []string{"other"}}))
	assert.Empty(t, match(jwt.MapClaims{}))
}

func TestTokenExchangeAudienceMatches(t *testing.T) {
	assert.True(t, tokenExchangeAudienceMatches(jwt.MapClaims{"aud": "admin"}, "admin"))
	assert.True(t, tokenExchangeAudienceMatches(jwt.MapClaims{"azp": "admin"}, "admin"))
	assert.True(t, tokenExchangeAudienceMatches(jwt.MapClaims{
		"aud": []string{"n8n", "admin"}, "azp": "admin",
	}, "admin"))

	// The token was issued to another client listed in aud
	assert.False(t, tokenExchangeAudienceMatches(jwt.MapClaims{
		"aud": []string{"admin", "n8n"}, "azp": "n8n",
	}, "admin"))
	assert.False(t, tokenExchangeAudienceMatches(jwt.MapClaims{"aud": []string{"admin", "n8n"}}, "admin"))
	assert.False(t, tokenExchangeAudienceMatches(jwt.MapClaims{"aud": "n8n"}, "admin"))
	assert.False(t, tokenExchangeAudienceMatches(jwt.MapClaims{}, "admin"))
}
