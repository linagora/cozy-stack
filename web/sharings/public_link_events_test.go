package sharings_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/require"
)

func TestSharedDrivePublicLinkEvents(t *testing.T) {
	f := setupSharedDrivePermissionFixture(t)
	f.env.acme.FeatureFlags = map[string]interface{}{publiclink.FlagEnabled: true}
	require.NoError(t, instance.Update(f.env.acme))
	eBetty, token := f.newBettyClient(t)
	// The fixture's owner URL is not an instance domain: exercise the real
	// HTTP reverse proxy, not the in-process same-stack shortcut.
	recipientSharing, err := sharing.FindSharing(f.env.betty, f.sharingID)
	require.NoError(t, err)
	ownerURL, err := url.Parse(recipientSharing.Members[0].Instance)
	require.NoError(t, err)
	_, err = lifecycle.GetInstance(ownerURL.Host)
	require.Error(t, err)
	eBetty = eBetty.Builder(func(r *httpexpect.Request) { r.WithHeader("X-Forwarded-For", "198.51.100.25") })
	payload := makeSharedDrivePermissionPayload(t, consts.Files, []string{f.fileID}, "", nil)
	id, attrs := createSharedDrivePermission(t, eBetty, f.sharingID, token, "link", "", payload)
	code := attrs.Value("codes").Object().Value("link").String().NotEmpty().Raw()
	readEvents := func() []publiclink.Event {
		var events []publiclink.Event
		require.NoError(t, couchdb.FindDocsUnoptimized(f.env.acme, consts.PublicLinkEvents, &couchdb.FindRequest{Selector: mango.Equal("link_id", id)}, &events))
		return events
	}
	require.Eventually(t, func() bool { return len(readEvents()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, publiclink.Created, readEvents()[0].Action)
	require.Equal(t, "198.51.100.25", readEvents()[0].IP)
	require.Equal(t, strings.TrimPrefix(f.env.tsB.URL, "http://"), readEvents()[0].Identity)
	patchSharedDrivePermissionExpectStatus(t, eBetty, f.sharingID, id, token, makeSharedDrivePatchPayload(t, map[string]interface{}{"expires_at": "2099-01-01T00:00:00Z"}), 200)
	f.eOwner.GET("/files/download/"+f.fileID).WithQuery("Dl", "1").WithHeader("Authorization", "Bearer "+code).Expect().Status(200)
	require.Eventually(t, func() bool { return len(readEvents()) == 3 }, 5*time.Second, 10*time.Millisecond)
	counts := map[publiclink.Action]int{}
	for _, event := range readEvents() {
		counts[event.Action]++
		if event.Action == publiclink.Updated {
			require.Equal(t, "198.51.100.25", event.IP)
			require.Equal(t, strings.TrimPrefix(f.env.tsB.URL, "http://"), event.Identity)
		}
		if event.Action == publiclink.Downloaded {
			require.Empty(t, event.Identity)
		}
	}
	require.Equal(t, map[publiclink.Action]int{publiclink.Created: 1, publiclink.Updated: 1, publiclink.Downloaded: 1}, counts)
	var recipientEvents []publiclink.Event
	require.NoError(t, couchdb.FindDocsUnoptimized(f.env.betty, consts.PublicLinkEvents, &couchdb.FindRequest{Selector: mango.Exists("link_id")}, &recipientEvents))
	require.Empty(t, recipientEvents, "events belong to the shared drive owner, not the creator")
	deleteSharedDrivePermissionExpectStatus(t, eBetty, f.sharingID, id, token, 204)
	require.Len(t, readEvents(), 3)
}
