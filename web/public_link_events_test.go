package web_test

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
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
	inst.FeatureFlags = map[string]interface{}{publiclink.FlagEnabled: true}
	require.NoError(t, instance.Update(inst))
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
	childID := makeFile("/shared/child.txt")
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
	linkID, token := link(rootID)
	dirLinkID, dirToken := link(dir.ID())
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

	for _, tc := range []struct {
		method, path, tok string
		headers           map[string]string
		status            int
	}{
		{"GET", "/files/" + rootID, token, nil, 200},
		{"GET", "/files/download/" + rootID, token, nil, 200},
		{"HEAD", "/files/download/" + rootID + "?Dl=1", token, nil, 200},
		{"GET", "/files/download/" + rootID + "?Dl=1", token, map[string]string{"Range": "bytes=0-2"}, 206},
		{"GET", "/files/download/" + childID + "?Dl=1", dirToken, nil, 200},
		{"GET", "/files/download/" + rootID + "?Dl=1", "", nil, 401},
	} {
		request(tc.method, tc.path, tc.tok, nil, tc.headers, tc.status)
	}
	require.Len(t, events(), 3)
	for n := 0; n < 2; n++ {
		b := request("GET", "/files/download/"+rootID+"?Dl=1", token, nil, map[string]string{"X-Forwarded-For": "192.0.2.55"}, 200)
		require.Equal(t, "public link content", string(b))
	}
	require.Len(t, events(), 5, "no deduplication")

	related := func(b []byte) string {
		t.Helper()
		var p struct{ Links struct{ Related string } }
		require.NoError(t, json.Unmarshal(b, &p))
		require.NotEmpty(t, p.Links.Related)
		return p.Links.Related
	}
	fileURL := related(request("POST", "/files/downloads?Id="+rootID, token, nil, nil, 200))
	require.Len(t, events(), 6, "preparing a secret records the intent even without a GET")
	require.Equal(t, "public link content", string(request("GET", fileURL+"?Dl=1", "", nil, nil, 200)))
	require.Equal(t, "public link content", string(request("GET", fileURL+"?Dl=1&sharecode="+token, "", nil, nil, 200)))
	require.Len(t, events(), 6, "reusing a secret with a sharecode must not count again")
	legacy, err := vfs.GetStore().AddFile(inst, "/root.txt")
	require.NoError(t, err)
	request("GET", "/files/downloads/"+legacy+"/file?Dl=1", token, nil, nil, 200)
	require.Len(t, events(), 6, "old secrets have no attributable link")

	archiveBody := []byte(`{"data":{"type":"io.cozy.files.archives","attributes":{"name":"shared","ids":["` + dir.ID() + `"]}}}`)
	archiveURL := related(request("POST", "/files/archive", dirToken, archiveBody, nil, 200))
	require.Len(t, events(), 7, "archive preparation counts before consumption")
	b := request("GET", archiveURL, "", nil, nil, 200)
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	require.NoError(t, err)
	require.Len(t, zr.File, 2)
	require.Len(t, events(), 7)
	request("POST", "/files/archive", dirToken, archiveBody, map[string]string{"Accept": "application/zip"}, 200)
	require.Len(t, events(), 8)
	request("GET", archiveURL, "", nil, map[string]string{"Range": "bytes=0-2"}, 200)
	selection := []byte(`{"data":{"type":"io.cozy.files.archives","attributes":{"name":"selection","ids":["` + childID + `"]}}}`)
	selectionURL := related(request("POST", "/files/archive", dirToken, selection, nil, 200))
	request("GET", selectionURL, "", nil, nil, 200)
	request("GET", "/files/archive/invalid/bad.zip", "", nil, nil, 400)
	require.Len(t, events(), 8)
	request("DELETE", "/permissions/"+linkID, ownerToken, nil, nil, 204)
	require.Len(t, events(), 8, "revocation is outside the event scope")
	counts := map[string]int{}
	for _, ev := range events() {
		counts[ev.LinkID]++
		if ev.Action == publiclink.Downloaded {
			require.Empty(t, ev.Identity)
		}
	}
	require.Equal(t, map[string]int{linkID: 5, dirLinkID: 3}, counts)
	// Existing capabilities keep their old access semantics after revocation.
	// Reusing them does not record another intent.
	request("GET", fileURL+"?Dl=1", "", nil, nil, 200)
	require.Len(t, events(), 8)

	t.Run("EmptyDirectory", func(t *testing.T) {
		empty, err := vfs.Mkdir(inst.VFS(), "/empty", nil)
		require.NoError(t, err)
		id, code := link(empty.ID())
		body := []byte(`{"data":{"type":"io.cozy.files.archives","attributes":{"name":"empty","ids":["` + empty.ID() + `"]}}}`)
		path := related(request("POST", "/files/archive", code, body, nil, 200))
		b := request("GET", path, "", nil, nil, 200)
		reader, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		require.NoError(t, err)
		require.Len(t, reader.File, 1)
		var actions []publiclink.Action
		for _, event := range events() {
			if event.LinkID == id {
				actions = append(actions, event.Action)
			}
		}
		require.ElementsMatch(t, []publiclink.Action{publiclink.Created, publiclink.Downloaded}, actions)
	})

	t.Run("PreparationScopeAndFlag", func(t *testing.T) {
		id, code := link(rootID)
		downloads := func() []publiclink.Event {
			var result []publiclink.Event
			for _, event := range events() {
				if event.LinkID == id && event.Action == publiclink.Downloaded {
					result = append(result, event)
				}
			}
			return result
		}
		before := len(events())
		request("POST", "/files/downloads?Id="+rootID, "", nil, nil, 401)
		request("POST", "/files/downloads?Id=missing", code, nil, nil, 404)
		request("POST", "/files/downloads?VersionId="+rootID+"/old", code, nil, nil, 200)
		request("POST", "/files/downloads?Id="+childID, dirToken, nil, nil, 200)
		require.Len(t, events(), before, "failed, version and child preparations are excluded")

		inst.FeatureFlags[publiclink.FlagEnabled] = false
		require.NoError(t, instance.Update(inst))
		path := related(request("POST", "/files/downloads?Id="+rootID, code, nil, nil, 200))
		require.Empty(t, downloads())
		inst.FeatureFlags[publiclink.FlagEnabled] = true
		require.NoError(t, instance.Update(inst))
		require.Equal(t, "public link content", string(request("GET", path+"?Dl=1", code, nil, nil, 200)))
		require.Empty(t, downloads(), "enabling later must not catch up an unrecorded preparation")

		path = related(request("POST", "/files/downloads?Id="+rootID, code, nil, map[string]string{"X-Forwarded-For": "192.0.2.77"}, 200))
		require.Len(t, downloads(), 1)
		require.Equal(t, "192.0.2.77", downloads()[0].IP)
		require.Empty(t, downloads()[0].Identity)
		inst.FeatureFlags[publiclink.FlagEnabled] = false
		require.NoError(t, instance.Update(inst))
		request("GET", path+"?Dl=1", code, nil, map[string]string{"X-Forwarded-For": "192.0.2.88"}, 200)
		require.Len(t, downloads(), 1)
		require.Equal(t, "192.0.2.77", downloads()[0].IP, "identity and IP belong to preparation")
		inst.FeatureFlags[publiclink.FlagEnabled] = true
		require.NoError(t, instance.Update(inst))
	})

	t.Run("ConditionalDownloadAttempt", func(t *testing.T) {
		id, code := link(rootID)
		request("GET", "/files/download/"+rootID+"?Dl=1", code, nil, map[string]string{"If-None-Match": "*"}, 304)
		var actions []publiclink.Action
		for _, event := range events() {
			if event.LinkID == id {
				actions = append(actions, event.Action)
			}
		}
		require.ElementsMatch(t, []publiclink.Action{publiclink.Created, publiclink.Downloaded}, actions)
	})

	t.Run("DownloadStart", func(t *testing.T) {
		networkDir, err := vfs.Mkdir(inst.VFS(), "/network", nil)
		require.NoError(t, err)
		f, err := vfs.Create(inst.VFS(), "/network/large.bin")
		require.NoError(t, err)
		const size = 16 * 1024 * 1024
		data := make([]byte, size)
		_, err = rand.Read(data)
		require.NoError(t, err)
		_, err = f.Write(data)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		doc, err := inst.VFS().FileByPath("/network/large.bin")
		require.NoError(t, err)
		id, code := link(doc.ID())
		archiveID, archiveToken := link(networkDir.ID())
		archivePath := related(request("POST", "/files/archive", archiveToken, []byte(`{"data":{"type":"io.cozy.files.archives","attributes":{"name":"network","ids":["`+networkDir.ID()+`"]}}}`), nil, 200))
		countDownloads := func(linkID string) int {
			n := 0
			for _, event := range events() {
				if event.LinkID == linkID && event.Action == publiclink.Downloaded {
					n++
				}
			}
			return n
		}
		for _, version := range []string{"HTTP/1.1", "HTTP/2.0"} {
			t.Run(version, func(t *testing.T) {
				server := httptest.NewUnstartedServer(ts.Config.Handler)
				server.EnableHTTP2 = version == "HTTP/2.0"
				server.StartTLS()
				t.Cleanup(server.Close)
				fetch := func(path, token string) *http.Response {
					req, err := http.NewRequest("GET", server.URL+path, nil)
					require.NoError(t, err)
					req.Host = inst.Domain
					req.Header.Set("Authorization", "Bearer "+token)
					res, err := server.Client().Do(req)
					require.NoError(t, err)
					require.Equal(t, version, res.Proto)
					require.Equal(t, 200, res.StatusCode)
					return res
				}
				before := countDownloads(id)
				res := fetch("/files/download/"+doc.ID()+"?Dl=1", code)
				n, err := io.Copy(io.Discard, res.Body)
				require.NoError(t, err)
				require.NoError(t, res.Body.Close())
				require.EqualValues(t, size, n)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("complete handler did not finish")
				}
				require.Equal(t, before+1, countDownloads(id))

				res = fetch("/files/download/"+doc.ID()+"?Dl=1", code)
				_, err = io.CopyN(io.Discard, res.Body, 65536)
				require.NoError(t, err)
				require.NoError(t, res.Body.Close())
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("aborted handler did not finish")
				}
				require.Equal(t, before+2, countDownloads(id), "an aborted download was started")

				archiveBefore := countDownloads(archiveID)
				res = fetch(archivePath, "")
				archiveData, err := io.ReadAll(res.Body)
				require.NoError(t, err)
				require.NoError(t, res.Body.Close())
				reader, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
				require.NoError(t, err)
				require.Len(t, reader.File, 2)
				entry, err := reader.File[1].Open()
				require.NoError(t, err)
				content, err := io.ReadAll(entry)
				require.NoError(t, err)
				require.NoError(t, entry.Close())
				require.Equal(t, data, content)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("archive handler did not finish")
				}
				require.Equal(t, archiveBefore, countDownloads(archiveID))

				res = fetch(archivePath, "")
				_, err = io.CopyN(io.Discard, res.Body, 65536)
				require.NoError(t, err)
				require.NoError(t, res.Body.Close())
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("aborted archive handler did not finish")
				}
				require.Equal(t, archiveBefore, countDownloads(archiveID), "aborting a prepared archive does not record another intent")
			})
		}
	})
}
