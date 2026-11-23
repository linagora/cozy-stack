package bitwarden

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrongPassphrasesAreCounted(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{Passphrase: "cozy"})

	old := limits.GetMaximumLimit(limits.AuthType)
	limits.SetMaximumLimit(limits.AuthType, 1)
	t.Cleanup(func() { limits.SetMaximumLimit(limits.AuthType, old) })

	connect := func() int {
		form := url.Values{"grant_type": {"password"}, "password": {"wrong"}}
		req := httptest.NewRequest(http.MethodPost, "/bitwarden/identity/connect/token", strings.NewReader(form.Encode()))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(req, rec)
		c.Set("instance", inst)
		require.NoError(t, getInitialCredentials(c))
		return rec.Code
	}
	isBlocked := func() bool {
		reloaded, err := instance.Get(inst.Domain)
		require.NoError(t, err)
		return reloaded.Blocked
	}

	assert.Equal(t, http.StatusUnauthorized, connect())
	assert.False(t, isBlocked())
	assert.Equal(t, http.StatusUnauthorized, connect())
	assert.True(t, isBlocked())
}
