package realtime

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
)

type testDoc struct {
	id      string
	doctype string
}

func TestRealtime(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient("io.cozy.foos io.cozy.bars io.cozy.bazs")
	ts := setup.GetTestServer("/realtime", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("RejectEmptyDocuments", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		for _, body := range []string{"", "null"} {
			e.POST("/realtime/io.cozy.foos/foo-one").
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(body)).
				Expect().Status(http.StatusBadRequest)
		}
	})

	t.Run("WSNoAuth", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		ws := e.GET("/realtime/").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		obj := ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.foos" }}`).
			Expect().TextMessage().
			JSON().Object()

		obj.ValueEqual("event", "error")
		payload := obj.Value("payload").Object()
		payload.ValueEqual("status", "405 Method Not Allowed")
		payload.ValueEqual("code", "method not allowed")
		payload.ValueEqual("title", "The SUBSCRIBE method is not supported")
	})

	t.Run("WSInvalidToken", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		ws := e.GET("/realtime/").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		obj := ws.WriteText(`{"method": "AUTH", "payload": "123456789"}`).
			Expect().TextMessage().
			JSON().Object()

		obj.ValueEqual("event", "error")
		payload := obj.Value("payload").Object()
		payload.ValueEqual("status", "401 Unauthorized")
		payload.ValueEqual("code", "unauthorized")
		payload.ValueEqual("title", "The authentication has failed")
	})

	t.Run("WSNoPermissionsForADoctype", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		ws := e.GET("/realtime/").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, token))

		obj := ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.contacts" }}`).
			Expect().TextMessage().
			JSON().Object()

		obj.ValueEqual("event", "error")
		payload := obj.Value("payload").Object()
		payload.ValueEqual("status", "403 Forbidden")
		payload.ValueEqual("code", "forbidden")
		payload.ValueEqual("title", "The application can't subscribe to io.cozy.contacts")
	})

	t.Run("WSForbiddenForRecents", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		_, recentsToken := setup.GetTestClient("io.cozy.files.recents io.cozy.foos")

		ws := e.GET("/realtime/").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, recentsToken))

		ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.foos" }}`)
		ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.files.recents" }}`)
		time.Sleep(30 * time.Millisecond)

		h := realtime.GetHub()
		h.Publish(inst, realtime.EventCreate, &testDoc{
			doctype: "io.cozy.files.recents",
			id:      "recents-one",
		}, nil)
		h.Publish(inst, realtime.EventCreate, &testDoc{
			doctype: "io.cozy.foos",
			id:      "foo-three",
		}, nil)

		obj := ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "error")
		payload := obj.Value("payload").Object()
		payload.ValueEqual("status", "403 Forbidden")
		payload.ValueEqual("code", "forbidden")
		payload.ValueEqual("title", "The application can't subscribe to io.cozy.files.recents")

		obj = ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "CREATED")
		obj.Value("payload").Object().ValueEqual("id", "foo-three")
	})

	t.Run("WSSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		ws := e.GET("/realtime/").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, token))

		ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.foos" }}`)
		ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.bars", "id": "bar-one"  }}`)
		ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.bars", "id": "bar-two" }}`)

		h := realtime.GetHub()
		time.Sleep(30 * time.Millisecond)

		h.Publish(inst, realtime.EventUpdate, &testDoc{
			doctype: "io.cozy.foos",
			id:      "foo-one",
		}, nil)

		obj := ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "UPDATED")
		payload := obj.Value("payload").Object()
		payload.ValueEqual("type", "io.cozy.foos")
		payload.ValueEqual("id", "foo-one")

		h.Publish(inst, realtime.EventDelete, &testDoc{
			doctype: "io.cozy.foos",
			id:      "foo-two",
		}, nil)

		obj = ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "DELETED")
		payload = obj.Value("payload").Object()
		payload.ValueEqual("type", "io.cozy.foos")
		payload.ValueEqual("id", "foo-two")

		h.Publish(inst, realtime.EventCreate, &testDoc{
			doctype: "io.cozy.bars",
			id:      "bar-three",
		}, nil)
		// No event

		h.Publish(inst, realtime.EventCreate, &testDoc{
			doctype: "io.cozy.bars",
			id:      "bar-one",
		}, nil)

		obj = ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "CREATED")
		payload = obj.Value("payload").Object()
		payload.ValueEqual("type", "io.cozy.bars")
		payload.ValueEqual("id", "bar-one")

		ws.WriteText(`{"method": "UNSUBSCRIBE", "payload": { "type": "io.cozy.bars", "id": "bar-one" }}`)
		time.Sleep(30 * time.Millisecond)

		h.Publish(inst, realtime.EventUpdate, &testDoc{
			doctype: "io.cozy.bars",
			id:      "bar-one",
		}, nil)
		// No event

		h.Publish(inst, realtime.EventUpdate, &testDoc{
			doctype: "io.cozy.bars",
			id:      "bar-two",
		}, nil)

		obj = ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "UPDATED")
		payload = obj.Value("payload").Object()
		payload.ValueEqual("type", "io.cozy.bars")
		payload.ValueEqual("id", "bar-two")
	})

	t.Run("WSNotify", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		ws := e.GET("/realtime/").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, token))

		ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.bazs", "id": "baz-one"  }}`)
		time.Sleep(30 * time.Millisecond)

		e.POST("/realtime/io.cozy.bazs/baz-one").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{ "hello": "world" }`)).
			Expect().Status(204)

		obj := ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "NOTIFIED")
		payload := obj.Value("payload").Object()
		payload.ValueEqual("type", "io.cozy.bazs")
		payload.ValueEqual("id", "baz-one")

		doc := payload.Value("doc").Object()
		doc.ValueEqual("hello", "world")
	})
}

func (t *testDoc) ID() string      { return t.id }
func (t *testDoc) DocType() string { return t.doctype }
func (t *testDoc) MarshalJSON() ([]byte, error) {
	j := `{"_id":"` + t.id + `", "_type":"` + t.doctype + `"}`
	return []byte(j), nil
}
