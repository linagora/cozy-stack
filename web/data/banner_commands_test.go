package data

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestBannerCommandStateIsPrivate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	ts := setup.GetTestServer("/data", Routes)
	t.Cleanup(ts.Close)
	state := &couchdb.JSONDoc{Type: consts.BannerCommands, M: M{"_id": "banner-billing", "revision": 42}}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, state))
	public := &couchdb.JSONDoc{Type: consts.Banners, M: M{"_id": "banner-billing", "dismissedAt": nil}}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, public))

	for _, scope := range []string{consts.BannerCommands + " " + consts.Banners, "io.cozy.banners.*"} {
		t.Run(scope, func(t *testing.T) {
			_, token := setup.GetTestClient(scope)
			e := testutils.CreateTestClient(t, ts.URL)
			path := "/data/" + consts.BannerCommands
			for _, req := range []struct {
				method, suffix string
				body           interface{}
			}{
				{"GET", "/banner-billing", nil},
				{"PUT", "/banner-billing", state.M},
				{"PUT", "/new-command", M{"revision": 999}},
				{"POST", "/", M{"revision": 999}},
				{"DELETE", "/banner-billing", nil},
				{"DELETE", "/", nil},
				{"GET", "/_all_docs", nil},
				{"POST", "/_find", M{"selector": M{}}},
				{"POST", "/_bulk_docs", M{"docs": []interface{}{state.M}}},
				{"POST", "/_bulk_get", M{"docs": []interface{}{M{"id": state.ID()}}}},
				{"GET", "/_changes", nil},
			} {
				r := e.Request(req.method, path+req.suffix).WithHeader("Authorization", "Bearer "+token)
				if req.method == "DELETE" && req.suffix == "/banner-billing" {
					r.WithQuery("rev", state.Rev())
				}
				if req.body != nil {
					r.WithJSON(req.body)
				}
				r.Expect().Status(403)
			}
			// Reserving the internal state must not remove public dismissal access.
			require.NoError(t, couchdb.GetDoc(inst, consts.Banners, public.ID(), public))
			public.M["dismissedAt"] = time.Now().UTC().Format(time.RFC3339)
			e.PUT("/data/"+consts.Banners+"/"+public.ID()).
				WithHeader("Authorization", "Bearer "+token).WithJSON(public.M).Expect().Status(200)
		})
	}
	var unchanged couchdb.JSONDoc
	require.NoError(t, couchdb.GetDoc(inst, consts.BannerCommands, state.ID(), &unchanged))
	require.Equal(t, state.Rev(), unchanged.Rev())
}
