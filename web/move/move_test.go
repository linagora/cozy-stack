package move_test

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	webmove "github.com/cozy/cozy-stack/web/move"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestAuthorize(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	config.GetConfig().Move.URL = "https://move.example.net/"

	handler := echo.New()
	handler.HTTPErrorHandler = errors.ErrorHandler
	group := handler.Group("/move", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("instance", inst)
			sess, err := session.New(inst, session.LongRun, "")
			require.NoError(t, err)
			c.Set("session", sess)
			return next(c)
		}
	})
	webmove.Routes(group)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	e := testutils.CreateTestClient(t, ts.URL)

	for _, redirect := range []string{
		"https://evil.example/callback/source",
		"http://move.example.net/callback/source",
		"https://move.example.net.evil.example/callback/source",
	} {
		e.GET("/move/authorize").
			WithQuery("state", "123").
			WithQuery("redirect_uri", redirect).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(400)
	}

	location := e.GET("/move/authorize").
		WithQuery("state", "123").
		WithQuery("redirect_uri", "https://move.example.net/callback/source").
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().Status(303).
		Header("Location").Raw()
	u, err := url.Parse(location)
	require.NoError(t, err)
	require.Equal(t, "move.example.net", u.Host)
	require.NotEmpty(t, u.Query().Get("code"))
}
