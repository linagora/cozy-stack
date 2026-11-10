package permissions

import (
	"encoding/csv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
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

type publicLinkStatisticsSetup struct {
	inst                                                                                  *instance.Instance
	link, other, sameFile, wholeType, nonLink                                             *permission.Permission
	token, wrongToken, fileOnlyToken, writeEventsToken, partialEventsToken, code, tempDir string
	request                                                                               func(*testing.T, string, string) *http.Response
}

func setupPublicLinkStatistics(t *testing.T) publicLinkStatisticsSetup {
	t.Helper()
	if testing.Short() {
		t.Skip("requires CouchDB and an instance")
	}
	config.UseTestFile(t)
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	client, token := setup.GetTestClient("io.cozy.files:GET:file-a io.cozy.public_links.events:GET")
	makeToken := func(scope string) string {
		t.Helper()
		token, err := inst.MakeJWT(consts.AccessTokenAudience, client.ClientID, scope, "", time.Now())
		require.NoError(t, err)
		return token
	}
	wrongToken := makeToken("io.cozy.files:GET:file-b io.cozy.public_links.events:GET")
	fileOnlyToken := makeToken("io.cozy.files:GET:file-a")
	writeEventsToken := makeToken("io.cozy.files:GET:file-a io.cozy.public_links.events:POST")
	partialEventsToken := makeToken("io.cozy.files:GET:file-a io.cozy.public_links.events:GET:some-event")
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
	request := func(t *testing.T, id, bearer string) *http.Response {
		t.Helper()
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
	return publicLinkStatisticsSetup{inst, link, other, sameFile, wholeType, nonLink, token, wrongToken, fileOnlyToken, writeEventsToken, partialEventsToken, code, tempDir, request}
}

func TestPublicLinkStatisticsExportsOnlyRequestedLink(t *testing.T) {
	setup := setupPublicLinkStatistics(t)
	resp := setup.request(t, setup.link.ID(), setup.token)
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
	sameFileResponse := setup.request(t, setup.sameFile.ID(), setup.token)
	require.Equal(t, http.StatusOK, sameFileResponse.StatusCode)
	sameFileRows, err := csv.NewReader(sameFileResponse.Body).ReadAll()
	require.NoError(t, err)
	require.Equal(t, [][]string{
		{"occurred_at", "IP", "identity", "action"},
		{"2026-01-02T03:04:05Z", "192.0.2.3", "", "downloaded"},
	}, sameFileRows)
}

func TestPublicLinkStatisticsRejectsUnauthorizedRequests(t *testing.T) {
	setup := setupPublicLinkStatistics(t)
	for _, tc := range []struct{ name, id, token string }{
		{"wrong token", setup.link.ID(), setup.wrongToken},
		{"file only", setup.link.ID(), setup.fileOnlyToken},
		{"same file only", setup.sameFile.ID(), setup.fileOnlyToken},
		{"events without get", setup.link.ID(), setup.writeEventsToken},
		{"partial events", setup.link.ID(), setup.partialEventsToken},
		{"other file", setup.other.ID(), setup.token},
		{"whole type", setup.wholeType.ID(), setup.token},
		{"not a link", setup.nonLink.ID(), setup.token},
		{"share code", setup.link.ID(), setup.code},
		{"no token", setup.link.ID(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := setup.request(t, tc.id, tc.token)
			if tc.token != "" && tc.token != setup.code {
				require.Equal(t, http.StatusForbidden, res.StatusCode)
			} else {
				require.NotEqual(t, http.StatusOK, res.StatusCode)
			}
			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.NotContains(t, string(body), "192.0.2.")
			require.NotContains(t, string(body), "=cmd")
		})
	}
}

func TestPublicLinkStatisticsTempFileCreationFailure(t *testing.T) {
	setup := setupPublicLinkStatistics(t)
	t.Setenv("TMPDIR", filepath.Join(setup.tempDir, "missing"))
	failureToCreate := setup.request(t, setup.link.ID(), setup.token)
	require.Equal(t, http.StatusInternalServerError, failureToCreate.StatusCode)
	failureToCreateBody, err := io.ReadAll(failureToCreate.Body)
	require.NoError(t, err)
	require.NotContains(t, string(failureToCreateBody), "192.0.2.1")
}

func TestPublicLinkStatisticsInvalidEventFailure(t *testing.T) {
	setup := setupPublicLinkStatistics(t)
	invalid := &couchdb.JSONDoc{Type: consts.PublicLinkEvents, M: map[string]interface{}{
		"_id": "invalid-event", "link_id": setup.link.ID(), "occurred_at": "invalid", "ip": "192.0.2.1",
	}}
	require.NoError(t, couchdb.CreateNamedDoc(setup.inst, invalid))
	failure := setup.request(t, setup.link.ID(), setup.token)
	require.Equal(t, http.StatusInternalServerError, failure.StatusCode)
	failureBody, err := io.ReadAll(failure.Body)
	require.NoError(t, err)
	require.NotContains(t, string(failureBody), "192.0.2.1")
	remaining, err := os.ReadDir(setup.tempDir)
	require.NoError(t, err)
	require.Empty(t, remaining)
}
