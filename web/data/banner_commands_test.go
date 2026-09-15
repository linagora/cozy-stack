package data

import (
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestBannerWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	ts := setup.GetTestServer("/data", Routes)
	_, token := setup.GetTestClient(consts.Banners)
	e := testutils.CreateTestClient(t, ts.URL)
	auth := "Bearer " + token

	create := func(id string, dismissible bool) *couchdb.JSONDoc {
		doc := &couchdb.JSONDoc{Type: consts.Banners, M: M{
			"_id": id, "revision": 42, "dismissible": dismissible, "dismissedAt": nil,
			"accepted": M{"text": M{"en": "Payment failed", "fr": "Échec du paiement"}},
		}}
		require.NoError(t, couchdb.CreateNamedDocWithDB(inst, doc))
		return doc
	}
	get := func(id string) couchdb.JSONDoc {
		var stored couchdb.JSONDoc
		require.NoError(t, couchdb.GetDoc(inst, consts.Banners, id, &stored))
		return stored
	}

	t.Run("a dismissal keeps every other field", func(t *testing.T) {
		public := create("banner-billing", true)
		path := "/data/" + consts.Banners + "/" + public.ID()
		e.GET(path).WithHeader("Authorization", auth).Expect().Status(200)
		previousRev := public.Rev()
		public.M["dismissedAt"] = "2026-01-01T00:00:00Z"
		public.M["revision"] = 1
		public.M["accepted"] = nil
		e.PUT(path).WithHeader("Authorization", auth).WithJSON(public.M).Expect().Status(200)
		public.SetRev(previousRev)
		e.PUT(path).WithHeader("Authorization", auth).WithJSON(public.M).Expect().Status(409)

		stored := get(public.ID())
		require.NotNil(t, stored.M["dismissedAt"])
		require.NotEqual(t, "2026-01-01T00:00:00Z", stored.M["dismissedAt"])
		require.EqualValues(t, 42, stored.M["revision"])
		require.Equal(t, "Échec du paiement", stored.M["accepted"].(map[string]interface{})["text"].(map[string]interface{})["fr"])
	})

	t.Run("a blocking banner cannot be closed", func(t *testing.T) {
		public := create("banner-trial", false)
		path := "/data/" + consts.Banners + "/" + public.ID()
		public.M["dismissedAt"] = "2026-01-01T00:00:00Z"
		public.M["dismissible"] = true
		e.PUT(path).WithHeader("Authorization", auth).WithJSON(public.M).Expect().Status(403)
		e.DELETE(path).WithQuery("rev", public.Rev()).WithHeader("Authorization", auth).Expect().Status(403)
		e.POST("/data/"+consts.Banners+"/_bulk_docs").WithHeader("Authorization", auth).
			WithJSON(M{"docs": []M{public.M}}).Expect().Status(403)

		stored := get(public.ID())
		require.Equal(t, public.Rev(), stored.Rev())
		require.Nil(t, stored.M["dismissedAt"])
	})

	t.Run("a client cannot author a banner", func(t *testing.T) {
		e.POST("/data/"+consts.Banners+"/").WithHeader("Authorization", auth).
			WithJSON(M{"dismissible": false}).Expect().Status(403)
		e.PUT("/data/"+consts.Banners+"/banner-account").WithHeader("Authorization", auth).
			WithJSON(M{"dismissible": false}).Expect().Status(400)
	})
}
