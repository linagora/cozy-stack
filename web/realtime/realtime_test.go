package realtime

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
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

	t.Run("NotifyOnFile", func(t *testing.T) {
		fs := inst.VFS()
		shared, err := vfs.Mkdir(fs, "/shared", nil)
		require.NoError(t, err)
		other, err := vfs.Mkdir(fs, "/other", nil)
		require.NoError(t, err)
		doc, err := vfs.NewFileDoc("drawing.txt", shared.ID(), -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := fs.CreateFile(doc, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		nested, err := vfs.MkdirAll(fs, "/shared/one/two")
		require.NoError(t, err)
		deepDoc, err := vfs.NewFileDoc("deep.txt", nested.ID(), -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err = fs.CreateFile(deepDoc, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		foreign := &couchdb.JSONDoc{Type: consts.Files, M: map[string]interface{}{"type": "io.cozy.foreign"}}
		foreign.SetID("foreign-doc")
		require.NoError(t, couchdb.CreateNamedDoc(inst, foreign))

		scopeToken := func(scope string) string {
			_, tok := setup.GetTestClient(scope)
			return tok
		}
		cases := []struct {
			name   string
			token  string
			id     string
			body   string
			status int
		}{
			{"WriteOnParentDir", scopeToken("io.cozy.files:ALL:" + shared.ID()), doc.ID(), `{"hello":"world"}`, http.StatusNoContent},
			{"PostOnlyOnParentDir", scopeToken("io.cozy.files:POST:" + shared.ID()), doc.ID(), `{"hello":"world"}`, http.StatusNoContent},
			{"WriteOnFile", scopeToken("io.cozy.files:ALL:" + doc.ID()), doc.ID(), `{"hello":"world"}`, http.StatusNoContent},
			{"WholeTypeOnAnyID", scopeToken("io.cozy.files"), "not-a-file", `{"hello":"world"}`, http.StatusNoContent},
			{"WriteOnAncestorDir", scopeToken("io.cozy.files:POST:" + shared.ID()), deepDoc.ID(), `{"hello":"world"}`, http.StatusNoContent},
			{"WriteOnOtherDirDeepFile", scopeToken("io.cozy.files:ALL:" + other.ID()), deepDoc.ID(), `{"hello":"world"}`, http.StatusForbidden},
			{"ReadOnAncestorDir", scopeToken("io.cozy.files:GET:" + shared.ID()), deepDoc.ID(), `{"hello":"world"}`, http.StatusForbidden},
			{"ReadOnWholeType", scopeToken("io.cozy.files:GET"), doc.ID(), `{"hello":"world"}`, http.StatusForbidden},
			{"WriteOnIDWithoutDocument", scopeToken("io.cozy.files:ALL:not-a-file"), "not-a-file", `{"hello":"world"}`, http.StatusForbidden},
			{"WriteOnOtherDirForeignDoc", scopeToken("io.cozy.files:ALL:" + other.ID()), foreign.ID(), `{"hello":"world"}`, http.StatusForbidden},
			{"ReadOnParentDir", scopeToken("io.cozy.files:GET:" + shared.ID()), doc.ID(), `{"hello":"world"}`, http.StatusForbidden},
			{"ReadOnFile", scopeToken("io.cozy.files:GET:" + doc.ID()), doc.ID(), `{"hello":"world"}`, http.StatusForbidden},
			{"WriteOnOtherDir", scopeToken("io.cozy.files:ALL:" + other.ID()), doc.ID(), `{"hello":"world"}`, http.StatusForbidden},
			{"WriteOnParentDirUnknownID", scopeToken("io.cozy.files:ALL:" + shared.ID()), "not-a-file", `{"hello":"world"}`, http.StatusForbidden},
			{"SelectorMatchedByBodyOnly", scopeToken("io.cozy.files:POST:image:class"), doc.ID(), `{"class":"image"}`, http.StatusForbidden},
			{"OtherDoctype", token, doc.ID(), `{"hello":"world"}`, http.StatusForbidden},
			{"NoToken", "", doc.ID(), `{"hello":"world"}`, http.StatusUnauthorized},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				e := testutils.CreateTestClient(t, ts.URL)
				req := e.POST("/realtime/io.cozy.files/"+tc.id).
					WithHeader("Content-Type", "application/json").
					WithBytes([]byte(tc.body))
				if tc.token != "" {
					req = req.WithHeader("Authorization", "Bearer "+tc.token)
				}
				req.Expect().Status(tc.status)
			})
		}

		t.Run("SubscribeToForeignDoc", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)
			ws := e.GET("/realtime/").
				WithWebsocketUpgrade().
				Expect().Status(http.StatusSwitchingProtocols).
				Websocket()
			defer ws.Disconnect()
			ws.WithReadTimeout(5 * time.Second)

			ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, scopeToken("io.cozy.files:GET:"+other.ID())))
			obj := ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.files", "id": "foreign-doc" }}`).
				Expect().TextMessage().
				JSON().Object()
			obj.ValueEqual("event", "error")
			obj.Value("payload").Object().ValueEqual("status", "403 Forbidden")
		})

		t.Run("WithoutTheVFSLock", func(t *testing.T) {
			mu := config.Lock().ReadWrite(inst, "vfs")
			require.NoError(t, mu.Lock())
			defer mu.Unlock()

			tok := scopeToken("io.cozy.files:POST:" + shared.ID())
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/realtime/io.cozy.files/"+deepDoc.ID(), strings.NewReader(`{"hello":"world"}`))
			require.NoError(t, err)
			req.Host = inst.Domain
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			client := http.Client{Timeout: 2 * time.Second}
			res, err := client.Do(req)
			require.NoError(t, err)
			defer res.Body.Close()
			require.Equal(t, http.StatusNoContent, res.StatusCode)
		})
	})
}

func (t *testDoc) ID() string      { return t.id }
func (t *testDoc) DocType() string { return t.doctype }
func (t *testDoc) MarshalJSON() ([]byte, error) {
	j := `{"_id":"` + t.id + `", "_type":"` + t.doctype + `"}`
	return []byte(j), nil
}
