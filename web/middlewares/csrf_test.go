package middlewares

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCsrf(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	config.GetConfig().Assets = "../../assets"
	setup := testutils.NewSetup(t, t.Name())

	setup.SetupSwiftTest()
	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()), "Could not init dynamic FS")

	t.Run("CSRF", func(t *testing.T) {
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		csrf := CSRFWithConfig(CSRFConfig{
			TokenLength: 16,
		})
		h := csrf(func(c echo.Context) error {
			return c.String(http.StatusOK, "test")
		})

		// Generate CSRF token
		assert.NoError(t, h(c))
		assert.Contains(t, rec.Header().Get(echo.HeaderSetCookie), "_csrf")

		// Without CSRF cookie
		req = httptest.NewRequest(http.MethodPost, "/", nil)
		rec = httptest.NewRecorder()
		c = e.NewContext(req, rec)
		assert.Error(t, h(c))

		// Empty/invalid CSRF token
		req = httptest.NewRequest(http.MethodPost, "/", nil)
		rec = httptest.NewRecorder()
		c = e.NewContext(req, rec)
		req.Header.Set(echo.HeaderXCSRFToken, "")
		assert.Error(t, h(c))

		// Valid CSRF token
		token := utils.RandomString(16)
		req.Header.Set(echo.HeaderCookie, "_csrf="+token)
		req.Header.Set(echo.HeaderXCSRFToken, token)
		if assert.NoError(t, h(c)) {
			assert.Equal(t, http.StatusOK, rec.Code)
		}
	})

	t.Run("CSRFTokenFromForm", func(t *testing.T) {
		f := make(url.Values)
		f.Set("csrf", "token")
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(f.Encode()))
		req.Header.Add(echo.HeaderContentType, echo.MIMEApplicationForm)
		c := e.NewContext(req, nil)
		token, err := csrfTokenFromForm("csrf")(c)
		if assert.NoError(t, err) {
			assert.Equal(t, "token", token)
		}
		_, err = csrfTokenFromForm("invalid")(c)
		assert.Error(t, err)
	})

	t.Run("CSRFTokenFromQuery", func(t *testing.T) {
		q := make(url.Values)
		q.Set("csrf", "token")
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Add(echo.HeaderContentType, echo.MIMEApplicationForm)
		req.URL.RawQuery = q.Encode()
		c := e.NewContext(req, nil)
		token, err := csrfTokenFromQuery("csrf")(c)
		if assert.NoError(t, err) {
			assert.Equal(t, "token", token)
		}
		_, err = csrfTokenFromQuery("invalid")(c)
		assert.Error(t, err)
		csrfTokenFromQuery("csrf")
	})
}

func TestCSRFSecureCookieOnlyFromThisHost(t *testing.T) {
	e := echo.New()
	h := CSRFWithConfig(CSRFConfig{
		TokenLookup:  "form:csrf_token",
		CookieSecure: true,
		CookiePath:   "/auth",
		CookieDomain: "example.org",
	})(func(c echo.Context) error {
		return c.String(http.StatusOK, "test")
	})
	post := func(cookie, token string) error {
		form := url.Values{"csrf_token": {token}}
		req := httptest.NewRequest(http.MethodPost, "/auth/authorize", strings.NewReader(form.Encode()))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
		req.Header.Set(echo.HeaderCookie, cookie)
		return h(e.NewContext(req, httptest.NewRecorder()))
	}

	rec := httptest.NewRecorder()
	require.NoError(t, h(e.NewContext(httptest.NewRequest(http.MethodGet, "/auth/login", nil), rec)))
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, "__Host-_csrf", cookies[0].Name)
	assert.Equal(t, "/", cookies[0].Path)
	assert.Empty(t, cookies[0].Domain)
	assert.True(t, cookies[0].Secure)

	// A _csrf cookie set by another host on the parent domain is ignored.
	var he *echo.HTTPError
	require.ErrorAs(t, post("_csrf=tossed", "tossed"), &he)
	assert.Equal(t, http.StatusForbidden, he.Code)

	assert.NoError(t, post("__Host-_csrf="+cookies[0].Value, cookies[0].Value))
}
