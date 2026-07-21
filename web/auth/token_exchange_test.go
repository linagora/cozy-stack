package auth

import (
	"net/url"
	"testing"

	"github.com/cozy/cozy-stack/model/app"
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
