package realtime

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
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

	t.Run("WSPrivatePublicLinkEvents", func(t *testing.T) {
		perms := &permission.Permission{Type: permission.TypeWebapp, SourceID: consts.Apps + "/private-test", Permissions: permission.Set{{Type: "*", Verbs: permission.ALL}}}
		require.NoError(t, couchdb.CreateDoc(inst, perms))
		adminToken, err := inst.MakeJWT(consts.AppAudience, "private-test", "", "", time.Now())
		require.NoError(t, err)
		e := testutils.CreateTestClient(t, ts.URL)
		ws := e.GET("/realtime/").WithWebsocketUpgrade().Expect().Status(http.StatusSwitchingProtocols).Websocket()
		defer ws.Disconnect()
		ws.WriteText(fmt.Sprintf(`{"method":"AUTH","payload":"%s"}`, adminToken))
		ws.WriteText(`{"method":"SUBSCRIBE","payload":{"type":"io.cozy.foos"}}`)
		for _, id := range []string{"", `,"id":"event-id"`} {
			obj := ws.WriteText(fmt.Sprintf(`{"method":"SUBSCRIBE","payload":{"type":"%s"%s}}`, consts.PublicLinkEvents, id)).
				Expect().TextMessage().JSON().Object()
			obj.ValueEqual("event", "error")
			obj.Value("payload").Object().ValueEqual("status", "403 Forbidden")
		}
		realtime.GetHub().Publish(inst, realtime.EventCreate, &testDoc{doctype: "io.cozy.foos", id: "control"}, nil)
		obj := ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "CREATED")
		obj.Value("payload").Object().ValueEqual("id", "control")
	})

	recentsAppToken := func(t *testing.T, slug string, set permission.Set) string {
		_, err := permission.CreateWebappSet(inst, slug, set, "1.0.0")
		require.NoError(t, err)
		tok, err := inst.MakeJWT(consts.AppAudience, slug, "", "", time.Now())
		require.NoError(t, err)
		return tok
	}

	t.Run("WSRecentsWithPermission", func(t *testing.T) {
		tok := recentsAppToken(t, "recents-reader", permission.Set{
			{Type: consts.FilesRecents, Verbs: permission.Verbs(permission.GET)},
		})
		e := testutils.CreateTestClient(t, ts.URL)
		ws := e.GET("/realtime/").
			WithWebsocketUpgrade().
			Expect().Status(http.StatusSwitchingProtocols).
			Websocket()
		defer ws.Disconnect()
		ws.WithReadTimeout(5 * time.Second)

		ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, tok))
		ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.files.recents" }}`)
		time.Sleep(30 * time.Millisecond)

		row := &recents.FileDirActivity{
			DocID:          "recents-row",
			LastActivityAt: "2026-11-06T10:00:00Z",
			Name:           "report.odt",
			Type:           "file",
			UpdatedAt:      "2026-11-06T10:00:00Z",
			DirID:          "dir-id",
			Path:           "/report.odt",
			SchemaVersion:  recents.SchemaVersion,
		}
		require.NoError(t, couchdb.CreateNamedDoc(inst, row))

		obj := ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "CREATED")
		payload := obj.Value("payload").Object()
		payload.ValueEqual("type", consts.FilesRecents)
		payload.ValueEqual("id", "recents-row")
		payload.Value("doc").Object().ValueEqual("name", "report.odt")

		// The event of another instance must not reach this subscription: the
		// next message is the update of the row of this instance.
		other := prefixer.NewPrefixer(0, "other.example.net", "other-prefix")
		realtime.GetHub().Publish(other, realtime.EventUpdate, &testDoc{
			doctype: consts.FilesRecents,
			id:      "other-instance-row",
		}, nil)

		row.Name = "renamed.odt"
		require.NoError(t, couchdb.UpdateDoc(inst, row))

		obj = ws.Expect().TextMessage().JSON().Object()
		obj.ValueEqual("event", "UPDATED")
		payload = obj.Value("payload").Object()
		payload.ValueEqual("type", consts.FilesRecents)
		payload.ValueEqual("id", "recents-row")
		payload.Value("doc").Object().ValueEqual("name", "renamed.odt")
	})

	t.Run("WSRecentsWithoutPermission", func(t *testing.T) {
		foos := permission.Rule{Type: "io.cozy.foos", Verbs: permission.Verbs(permission.GET)}
		cases := []struct {
			name string
			set  permission.Set
		}{
			{"OtherDoctype", permission.Set{foos}},
			{"WholeFiles", permission.Set{foos, {Type: consts.Files, Verbs: permission.ALL}}},
			{"NoRead", permission.Set{foos, {Type: consts.FilesRecents, Verbs: permission.Verbs(permission.POST)}}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tok := recentsAppToken(t, "recents-denied-"+strings.ToLower(tc.name), tc.set)
				e := testutils.CreateTestClient(t, ts.URL)
				ws := e.GET("/realtime/").
					WithWebsocketUpgrade().
					Expect().Status(http.StatusSwitchingProtocols).
					Websocket()
				defer ws.Disconnect()
				ws.WithReadTimeout(5 * time.Second)

				ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, tok))
				ws.WriteText(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.foos" }}`)
				for _, id := range []string{"", `, "id": "recents-row"`} {
					obj := ws.WriteText(fmt.Sprintf(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.files.recents"%s }}`, id)).
						Expect().TextMessage().JSON().Object()
					obj.ValueEqual("event", "error")
					payload := obj.Value("payload").Object()
					payload.ValueEqual("status", "403 Forbidden")
					payload.ValueEqual("title", "The application can't subscribe to io.cozy.files.recents")
				}

				h := realtime.GetHub()
				h.Publish(inst, realtime.EventUpdate, &testDoc{doctype: consts.FilesRecents, id: "recents-row"}, nil)
				h.Publish(inst, realtime.EventCreate, &testDoc{doctype: "io.cozy.foos", id: "control"}, nil)
				obj := ws.Expect().TextMessage().JSON().Object()
				obj.ValueEqual("event", "CREATED")
				obj.Value("payload").Object().ValueEqual("id", "control")
			})
		}
	})

	t.Run("WSRecentsWithShareCode", func(t *testing.T) {
		shared, err := vfs.Mkdir(inst.VFS(), "/recents-shared", nil)
		require.NoError(t, err)
		s := &sharing.Sharing{
			Active: true,
			Owner:  true,
			Rules: []sharing.Rule{
				{Title: "recents-shared", DocType: consts.Files, Values: []string{shared.ID()}},
			},
			Members: []sharing.Member{
				{Status: sharing.MemberStatusOwner, Email: "owner@example.net"},
				{Status: sharing.MemberStatusReady, Email: "bob@example.net"},
			},
		}
		require.NoError(t, couchdb.CreateDoc(inst, s))

		preview, err := s.CreatePreviewPermissions(inst)
		require.NoError(t, err)
		interact, err := s.GetInteractCode(inst, &s.Members[1], 1)
		require.NoError(t, err)
		linkApp := &couchdb.JSONDoc{Type: consts.Apps, M: map[string]interface{}{"slug": "recents-link-app"}}
		linkApp.SetID(consts.Apps + "/recents-link-app")
		require.NoError(t, couchdb.CreateNamedDoc(inst, linkApp))
		parent := &permission.Permission{
			Type:     permission.TypeWebapp,
			SourceID: consts.Apps + "/recents-link-app",
			Permissions: permission.Set{
				{Type: consts.Files, Verbs: permission.ALL},
			},
		}
		link, err := inst.CreateShareCode("recents-link", "recents-link-perm")
		require.NoError(t, err)
		_, err = permission.CreateShareSet(inst, parent, parent.SourceID, map[string]string{"link": link}, nil, permission.Permission{
			PID: "recents-link-perm",
			Permissions: permission.Set{
				{Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{shared.ID()}},
			},
		}, nil, false)
		require.NoError(t, err)

		cases := []struct {
			name string
			code string
		}{
			{"SharePreview", preview.Codes["bob@example.net"]},
			{"ShareInteract", interact},
			{"ShareByLink", link},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				require.NotEmpty(t, tc.code)
				e := testutils.CreateTestClient(t, ts.URL)
				ws := e.GET("/realtime/").
					WithWebsocketUpgrade().
					Expect().Status(http.StatusSwitchingProtocols).
					Websocket()
				defer ws.Disconnect()
				ws.WithReadTimeout(5 * time.Second)

				ws.WriteText(fmt.Sprintf(`{"method": "AUTH", "payload": "%s"}`, tc.code))
				ws.WriteText(fmt.Sprintf(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.files", "id": "%s" }}`, shared.ID()))
				for _, id := range []string{"", `, "id": "recents-row"`} {
					obj := ws.WriteText(fmt.Sprintf(`{"method": "SUBSCRIBE", "payload": { "type": "io.cozy.files.recents"%s }}`, id)).
						Expect().TextMessage().JSON().Object()
					obj.ValueEqual("event", "error")
					payload := obj.Value("payload").Object()
					payload.ValueEqual("status", "403 Forbidden")
					payload.ValueEqual("title", "The application can't subscribe to io.cozy.files.recents")
				}

				// The code is valid: it still receives the events of its files,
				// and these events are the only ones it receives.
				h := realtime.GetHub()
				h.Publish(inst, realtime.EventUpdate, &testDoc{doctype: consts.FilesRecents, id: "recents-row"}, nil)
				h.Publish(inst, realtime.EventUpdate, &testDoc{doctype: consts.Files, id: shared.ID()}, nil)
				obj := ws.Expect().TextMessage().JSON().Object()
				obj.ValueEqual("event", "UPDATED")
				payload := obj.Value("payload").Object()
				payload.ValueEqual("type", consts.Files)
				payload.ValueEqual("id", shared.ID())
			})
		}
	})

	t.Run("NotifyOnRecentsStaysClosed", func(t *testing.T) {
		_, tok := setup.GetTestClient(consts.FilesRecents)
		e := testutils.CreateTestClient(t, ts.URL)
		e.POST("/realtime/"+consts.FilesRecents+"/recents-row").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+tok).
			WithBytes([]byte(`{"name":"forged"}`)).
			Expect().Status(http.StatusBadRequest)
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
