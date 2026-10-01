package ai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
)

func TestCreateMeeting(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	setup.GetTestInstance(&lifecycle.Options{Email: "alice@example.net"})
	_, token := setup.GetTestClient(consts.ChatConversations)
	_, filesToken := setup.GetTestClient(consts.Files)

	ts := setup.GetTestServer("/ai", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	fakeMeet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/external-api/v1.0/application/token/" {
			_, _ = w.Write([]byte(`{"access_token": "jwt", "token_type": "Bearer"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": "room-1", "slug": "abc-defg-hij", "url": "https://meet.example.net/abc-defg-hij"}`))
	}))
	t.Cleanup(fakeMeet.Close)

	servers := config.GetConfig().MeetServers
	t.Cleanup(func() { config.GetConfig().MeetServers = servers })

	t.Run("without a Meet server", func(t *testing.T) {
		config.GetConfig().MeetServers = nil
		e := testutils.CreateTestClient(t, ts.URL)
		e.POST("/ai/meetings").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)
	})

	config.GetConfig().MeetServers = map[string]config.MeetServer{
		config.DefaultInstanceContext: {URL: fakeMeet.URL, ClientID: "app-1", ClientSecret: "s3cr3t"},
	}

	t.Run("without the permission of the assistant", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		e.POST("/ai/meetings").
			WithHeader("Authorization", "Bearer "+filesToken).
			Expect().Status(403)
	})

	t.Run("creates a room", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		obj := e.POST("/ai/meetings").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON().Object()
		obj.Value("url").String().IsEqual("https://meet.example.net/abc-defg-hij")
		obj.Value("slug").String().IsEqual("abc-defg-hij")
	})
}
