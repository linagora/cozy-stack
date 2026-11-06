package permissions

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	webErrors "github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestActivityReportHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("requires an instance")
	}
	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient("io.cozy.files:GET")
	server := setup.GetTestServer("/permissions", Routes)
	server.Config.Handler.(*echo.Echo).HTTPErrorHandler = webErrors.ErrorHandler
	request := func(method, path string, attrs map[string]interface{}, bearer string) (int, map[string]interface{}) {
		body, err := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"type": consts.Permissions, "attributes": attrs}})
		require.NoError(t, err)
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		require.NoError(t, err)
		req.Host = inst.Domain
		req.Header.Set("Content-Type", "application/vnd.api+json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		var out map[string]interface{}
		if len(raw) > 0 && json.Valid(raw) {
			require.NoError(t, json.Unmarshal(raw, &out))
		}
		return resp.StatusCode, out
	}
	rules := map[string]interface{}{"files": map[string]interface{}{"type": consts.Files, "verbs": []string{"GET"}, "values": []string{"file-a"}}}
	for _, invalid := range []interface{}{nil, "true", 1} {
		status, _ := request("POST", "/permissions?codes=link", map[string]interface{}{"permissions": rules, "activity_report_enabled": invalid}, token)
		require.Equal(t, http.StatusUnprocessableEntity, status)
	}
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	status, out := request("POST", "/permissions?codes=link", map[string]interface{}{"permissions": rules, "activity_report_enabled": true, "expires_at": expiry, "password": "secret"}, token)
	require.Equal(t, http.StatusOK, status)
	id := out["data"].(map[string]interface{})["id"].(string)
	p, err := permission.GetPermissionByIDIncludingExpired(inst, id)
	require.NoError(t, err)
	require.True(t, *p.ActivityReportEnabled)
	savedPassword := p.Password
	status, _ = request("PATCH", "/permissions/"+id, map[string]interface{}{"activity_report_enabled": false}, token)
	require.Equal(t, http.StatusOK, status)
	p, err = permission.GetPermissionByIDIncludingExpired(inst, id)
	require.NoError(t, err)
	require.False(t, *p.ActivityReportEnabled)
	require.Equal(t, expiry, p.ExpiresAt)
	require.Equal(t, savedPassword, p.Password)
	status, _ = request("PATCH", "/permissions/"+id, map[string]interface{}{"password": ""}, token)
	require.Equal(t, http.StatusOK, status)
	p, err = permission.GetPermissionByIDIncludingExpired(inst, id)
	require.NoError(t, err)
	require.False(t, *p.ActivityReportEnabled)
	status, _ = request("PATCH", "/permissions/"+id, map[string]interface{}{"activity_report_enabled": nil}, token)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	status, _ = request("PATCH", "/permissions/"+id, map[string]interface{}{"activity_report_enabled": true}, p.Codes["link"])
	require.NotEqual(t, http.StatusOK, status)
}
