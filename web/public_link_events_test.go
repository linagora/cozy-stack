package web_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/stack"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/apps"
	weberrors "github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestPublicLinkEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and Redis")
	}
	config.UseTestFile(t)
	config.GetConfig().Assets = "../assets"
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, ownerToken := setup.GetTestClient(consts.Files)
	require.NoError(t, couchdb.CreateDB(inst, consts.PublicLinkEvents))
	done := make(chan struct{}, 100)
	api := echo.New()
	api.HTTPErrorHandler = weberrors.ErrorHandler
	api.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error { defer func() { done <- struct{}{} }(); return next(c) }
	})
	router, err := web.CreateSubdomainProxy(api, &stack.Services{}, apps.Serve)
	require.NoError(t, err)
	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)
	request := func(method, path, token string, body []byte, headers map[string]string, status int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		require.NoError(t, err)
		req.Host = inst.Domain
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handler did not finish")
		}
		require.Equal(t, status, res.StatusCode, string(b))
		return b
	}
	makeFile := func(path string) string {
		t.Helper()
		f, err := vfs.Create(inst.VFS(), path)
		require.NoError(t, err)
		_, err = io.WriteString(f, "public link content")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		doc, err := inst.VFS().FileByPath(path)
		require.NoError(t, err)
		return doc.ID()
	}
	rootID := makeFile("/root.txt")
	dir, err := vfs.Mkdir(inst.VFS(), "/shared", nil)
	require.NoError(t, err)
	makeFile("/shared/child.txt")
	events := func() []publiclink.Event {
		t.Helper()
		var ev []publiclink.Event
		require.NoError(t, couchdb.FindDocsUnoptimized(inst, consts.PublicLinkEvents, &couchdb.FindRequest{Selector: mango.Exists("link_id")}, &ev))
		return ev
	}
	link := func(id string) (string, string) {
		t.Helper()
		b := request("POST", "/permissions?codes=link", ownerToken, []byte(`{"data":{"type":"io.cozy.permissions","attributes":{"permissions":{"files":{"type":"io.cozy.files","verbs":["GET"],"values":["`+id+`"]}}}}}`), map[string]string{"X-Forwarded-For": "2001:db8::123, 192.0.2.1"}, 200)
		var payload struct {
			Data struct {
				ID         string
				Attributes struct{ Codes map[string]string }
			}
		}
		require.NoError(t, json.Unmarshal(b, &payload))
		return payload.Data.ID, payload.Data.Attributes.Codes["link"]
	}
	linkID, _ := link(rootID)
	link(dir.ID())
	require.Len(t, events(), 2)
	for _, ev := range events() {
		require.Equal(t, publiclink.Created, ev.Action)
		require.Equal(t, "2001:db8::123", ev.IP)
		require.Equal(t, inst.Domain, ev.Identity)
		require.WithinDuration(t, time.Now(), ev.OccurredAt, 5*time.Second)
	}

	// Successful updates, not failed updates or revocation, are link actions.
	request("PATCH", "/permissions/"+linkID, ownerToken, []byte(`{"data":{"type":"io.cozy.permissions","attributes":{"expires_at":"2099-01-01T00:00:00Z"}}}`), nil, 200)
	request("PATCH", "/permissions/"+linkID, ownerToken, []byte(`{"data":{"type":"io.cozy.permissions","attributes":{"expires_at":"invalid"}}}`), nil, 422)
	require.Len(t, events(), 3)

}
