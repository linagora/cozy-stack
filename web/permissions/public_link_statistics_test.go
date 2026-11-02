package permissions

import (
	"encoding/csv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	webErrors "github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestPublicLinkStatistics(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and an instance")
	}
	config.UseTestFile(t)
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	client, token := setup.GetTestClient("io.cozy.files:GET:file-a")
	wrongToken, err := inst.MakeJWT(consts.AccessTokenAudience, client.ClientID, "io.cozy.files:GET:file-b", "", time.Now())
	require.NoError(t, err)
	server := setup.GetTestServer("/permissions", Routes)
	server.Config.Handler.(*echo.Echo).HTTPErrorHandler = webErrors.ErrorHandler

	link := &permission.Permission{Type: permission.TypeShareByLink, SourceID: consts.OAuthClients + "/" + client.ClientID, Permissions: permission.Set{{Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{"file-a"}}}}
	require.NoError(t, couchdb.CreateDoc(inst, link))
	code, err := inst.CreateShareCode("public", link.ID())
	require.NoError(t, err)
	link.Codes = map[string]string{"public": code}
	require.NoError(t, couchdb.UpdateDoc(inst, link))
	other := &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{{Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{"file-b"}}}}
	require.NoError(t, couchdb.CreateDoc(inst, other))
	sameFile := &permission.Permission{Type: permission.TypeShareByLink, Permissions: link.Permissions}
	require.NoError(t, couchdb.CreateDoc(inst, sameFile))
	wholeType := &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{{Type: consts.Files, Verbs: permission.Verbs(permission.GET)}}}
	require.NoError(t, couchdb.CreateDoc(inst, wholeType))
	nonLink := &permission.Permission{Type: permission.TypeWebapp, Permissions: link.Permissions}
	require.NoError(t, couchdb.CreateDoc(inst, nonLink))
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, event := range []*publiclink.Event{
		{LinkID: link.ID(), Action: publiclink.Created, OccurredAt: at, IP: "192.0.2.1"},
		{LinkID: link.ID(), Action: publiclink.Opened, OccurredAt: at.Add(time.Hour), IP: "2001:db8::1", Identity: "=cmd"},
		{LinkID: other.ID(), Action: publiclink.Downloaded, OccurredAt: at, IP: "192.0.2.2"},
		{LinkID: sameFile.ID(), Action: publiclink.Downloaded, OccurredAt: at, IP: "192.0.2.3"},
	} {
		require.NoError(t, couchdb.CreateDoc(inst, event))
	}
	request := func(id, bearer string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/permissions/"+id+"/statistics", nil)
		require.NoError(t, err)
		req.Host = inst.Domain
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	resp := request(link.ID(), token)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/csv; charset=utf-8", resp.Header.Get("Content-Type"))
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	rows, err := csv.NewReader(resp.Body).ReadAll()
	require.NoError(t, err)
	require.Equal(t, [][]string{
		{"occurred_at", "IP", "identity", "action"},
		{"2026-01-02T03:04:05Z", "192.0.2.1", "", "created"},
		{"2026-01-02T04:04:05Z", "2001:db8::1", "'=cmd", "opened"},
	}, rows)
	sameFileResponse := request(sameFile.ID(), token)
	require.Equal(t, http.StatusOK, sameFileResponse.StatusCode)
	sameFileRows, err := csv.NewReader(sameFileResponse.Body).ReadAll()
	require.NoError(t, err)
	require.Equal(t, [][]string{
		{"occurred_at", "IP", "identity", "action"},
		{"2026-01-02T03:04:05Z", "192.0.2.3", "", "downloaded"},
	}, sameFileRows)
	for _, tc := range []struct{ id, token string }{
		{link.ID(), wrongToken}, {other.ID(), token}, {wholeType.ID(), token}, {nonLink.ID(), token}, {link.ID(), code}, {link.ID(), ""},
	} {
		res := request(tc.id, tc.token)
		require.NotEqual(t, http.StatusOK, res.StatusCode)
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NotContains(t, string(body), "192.0.2.1")
	}

	t.Setenv("TMPDIR", filepath.Join(tempDir, "missing"))
	failureToCreate := request(link.ID(), token)
	require.Equal(t, http.StatusInternalServerError, failureToCreate.StatusCode)
	failureToCreateBody, err := io.ReadAll(failureToCreate.Body)
	require.NoError(t, err)
	require.NotContains(t, string(failureToCreateBody), "192.0.2.1")
	t.Setenv("TMPDIR", tempDir)

	invalid := &couchdb.JSONDoc{Type: consts.PublicLinkEvents, M: map[string]interface{}{
		"_id": "invalid-event", "link_id": link.ID(), "occurred_at": "invalid", "ip": "192.0.2.1",
	}}
	require.NoError(t, couchdb.CreateNamedDoc(inst, invalid))
	failure := request(link.ID(), token)
	require.Equal(t, http.StatusInternalServerError, failure.StatusCode)
	failureBody, err := io.ReadAll(failure.Body)
	require.NoError(t, err)
	require.NotContains(t, string(failureBody), "192.0.2.1")
	remaining, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	require.Empty(t, remaining)
}
