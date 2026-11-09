package permissions

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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

func TestPublicLinkStatisticsConcurrentPages(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and an instance")
	}
	config.UseTestFile(t)
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient("io.cozy.files:GET:file-a")
	link := &permission.Permission{Type: permission.TypeShareByLink, Permissions: permission.Set{{Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{"file-a"}}}}
	require.NoError(t, couchdb.CreateDoc(inst, link))
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 1002; i++ {
		require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: link.ID(), Action: publiclink.Opened, OccurredAt: at, IP: fmt.Sprintf("192.0.2.%d", i)}))
	}
	server := setup.GetTestServer("/permissions", Routes)
	server.Config.Handler.(*echo.Echo).HTTPErrorHandler = webErrors.ErrorHandler
	type result struct {
		body []byte
		err  error
	}
	results := make(chan result, 4)
	for i := 0; i < cap(results); i++ {
		go func() {
			req, err := http.NewRequest(http.MethodGet, server.URL+"/permissions/"+link.ID()+"/statistics", nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Host = inst.Domain
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err == nil && resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
			}
			results <- result{body: body, err: err}
		}()
	}
	var first []byte
	for i := 0; i < cap(results); i++ {
		got := <-results
		require.NoError(t, got.err)
		rows, err := csv.NewReader(strings.NewReader(string(got.body))).ReadAll()
		require.NoError(t, err)
		require.Len(t, rows, 1003)
		require.Equal(t, []string{"occurred_at", "IP", "identity", "action"}, rows[0])
		seen := make(map[string]bool)
		for _, row := range rows[1:] {
			require.Equal(t, "2026-01-02T03:04:05Z", row[0])
			require.False(t, seen[row[1]])
			seen[row[1]] = true
		}
		if first == nil {
			first = got.body
		} else {
			require.Equal(t, first, got.body)
		}
	}
	invalid := &couchdb.JSONDoc{Type: consts.PublicLinkEvents, M: map[string]interface{}{
		"_id": "late-invalid", "link_id": link.ID(), "occurred_at": "2026-01-02T04:04:05Z", "ip": 123,
	}}
	require.NoError(t, couchdb.CreateNamedDoc(inst, invalid))
	body := testutils.CreateTestClient(t, server.URL).
		GET("/permissions/"+link.ID()+"/statistics").
		WithHost(inst.Domain).
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(http.StatusInternalServerError).Body()
	body.NotContains("192.0.2.")
	body.NotContains("occurred_at,IP,identity,action")
	remaining, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	require.Empty(t, remaining)
}
