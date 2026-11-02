package sharings_test

import (
	"encoding/csv"
	"net/http"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

func TestSharedDrivePublicLinkStatisticsOwner(t *testing.T) {
	f := setupSharedDrivePermissionFixture(t)
	eBetty, bettyToken := f.newBettyClient(t)
	payload := makeSharedDrivePermissionPayload(t, consts.Files, []string{f.fileID}, "", nil)
	linkID, _ := createSharedDrivePermission(t, eBetty, f.sharingID, bettyToken, "public", "", payload)

	link, err := permission.GetPermissionByIDIncludingExpired(f.env.acme, linkID)
	require.NoError(t, err)
	require.Equal(t, permission.TypeShareByLink, link.Type)
	require.NotEmpty(t, link.Codes["public"])
	event := &publiclink.Event{LinkID: linkID, Action: publiclink.Opened, OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), IP: "192.0.2.1"}
	require.NoError(t, couchdb.CreateDoc(f.env.acme, event))

	request := func(token string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, f.env.tsA.URL+"/permissions/"+linkID+"/statistics", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	resp := request(f.ownerAppToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rows, err := csv.NewReader(resp.Body).ReadAll()
	require.NoError(t, err)
	require.Equal(t, [][]string{{"occurred_at", "IP", "identity", "action"}, {"2026-01-02T03:04:05Z", "192.0.2.1", "", "opened"}}, rows)

	recipientSharing, err := sharing.FindSharing(f.env.betty, f.sharingID)
	require.NoError(t, err)
	require.NotEmpty(t, recipientSharing.Credentials)
	interactToken := recipientSharing.Credentials[0].DriveToken
	require.NotEmpty(t, interactToken)
	for _, token := range []string{interactToken, link.Codes["public"], bettyToken} {
		require.NotEqual(t, http.StatusOK, request(token).StatusCode)
	}
}
