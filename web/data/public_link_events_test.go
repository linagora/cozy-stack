package data

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestPublicLinkEventsPrivate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	perms := &permission.Permission{Type: permission.TypeWebapp, SourceID: consts.Apps + "/private-test", Permissions: permission.Set{{Type: "*", Verbs: permission.ALL}}}
	require.NoError(t, couchdb.CreateDoc(inst, perms))
	token, err := inst.MakeJWT(consts.AppAudience, "private-test", "", "", time.Now())
	require.NoError(t, err)
	event := &publiclink.Event{LinkID: "link-a", Action: publiclink.Opened, OccurredAt: time.Now().UTC(), IP: "192.0.2.1"}
	require.NoError(t, couchdb.CreateDoc(inst, event))
	control := &couchdb.JSONDoc{Type: "io.cozy.foos", M: map[string]interface{}{"_id": "control", "value": "visible"}}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, control))
	ts := setup.GetTestServer("/data", Routes)
	t.Cleanup(ts.Close)
	e := testutils.CreateTestClient(t, ts.URL)
	e.GET("/data/io.cozy.foos/control").WithHeader("Authorization", "Bearer "+token).Expect().Status(200).JSON().Object().ValueEqual("value", "visible")
	e.GET("/data/"+consts.PublicLinkEvents+"/"+event.ID()).WithHeader("Authorization", "Bearer "+token).Expect().Status(403)
	e.PUT("/data/"+consts.PublicLinkEvents+"/"+event.ID()).WithHeader("Authorization", "Bearer "+token).WithHeader("Content-Type", "application/json").WithJSON(map[string]string{"ip": "changed"}).Expect().Status(403)
	var got publiclink.Event
	require.NoError(t, couchdb.GetDoc(inst, consts.PublicLinkEvents, event.ID(), &got))
	require.Equal(t, event.IP, got.IP)
}
