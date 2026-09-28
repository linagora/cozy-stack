package data

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

type M map[string]interface{}
type S []interface{}

func TestData(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	const Type = "io.cozy.events"
	const ID = "4521C325F6478E45"

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	testInstance := setup.GetTestInstance()
	scope := "io.cozy.doctypes io.cozy.files io.cozy.events " +
		"io.cozy.anothertype io.cozy.nottype"

	_, token := setup.GetTestClient(scope)
	ts := setup.GetTestServer("/data", Routes)
	t.Cleanup(ts.Close)

	_ = couchdb.ResetDB(testInstance, Type)
	_ = couchdb.CreateNamedDoc(testInstance, &couchdb.JSONDoc{
		Type: Type,
		M: map[string]interface{}{
			"_id":  ID,
			"test": "testvalue",
		},
	})

	t.Run("SuccessGet", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/data/"+Type+"/"+ID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object().
			ValueEqual("test", "testvalue")
	})

	t.Run("GetWithRevisions", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			scope  string
			status int
		}{
			{"Unauthenticated", "", 401},
			{"WrongDoctype", "io.cozy.anothertype", 403},
			{"WriteOnly", Type + ":PUT", 403},
			{"WrongDocument", Type + ":GET:another-id", 403},
			{"WrongSelectorValue", Type + ":GET:anothervalue:test", 403},
			{"WholeDoctype", Type + ":GET", 200},
			{"DocumentID", Type + ":GET:" + ID, 200},
			{"SelectorValue", Type + ":GET:testvalue:test", 200},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := testutils.CreateTestClient(t, ts.URL)
				req := e.GET("/data/"+Type+"/"+ID).WithQuery("revs", "true")
				if tc.scope != "" {
					_, scopedToken := setup.GetTestClient(tc.scope)
					req.WithHeader("Authorization", "Bearer "+scopedToken)
				}
				resp := req.Expect().Status(tc.status)
				if tc.status == 200 {
					obj := resp.JSON().Object()
					obj.ValueEqual("_id", ID).ValueEqual("test", "testvalue")
					obj.Value("_revisions").Object().Value("ids").Array().NotEmpty()
				}
			})
		}
	})

	t.Run("GetBitwardenSettingsWithRevisions", func(t *testing.T) {
		require.NoError(t, couchdb.EnsureDBExist(testInstance, consts.Settings))
		_ = couchdb.CreateNamedDocWithDB(testInstance, &couchdb.JSONDoc{
			Type: consts.Settings,
			M: map[string]interface{}{
				"_id": consts.BitwardenSettingsID,
				"key": "bitwarden-vault-key",
			},
		})

		// A client holding only a permission on the bitwarden organizations
		// doctype can read the bitwarden settings document, with or without
		// the revisions.
		_, bitwardenToken := setup.GetTestClient(consts.BitwardenOrganizations + ":GET")

		e := testutils.CreateTestClient(t, ts.URL)
		e.GET("/data/"+consts.Settings+"/"+consts.BitwardenSettingsID).
			WithHeader("Authorization", "Bearer "+bitwardenToken).
			Expect().Status(200).
			JSON().Object().ValueEqual("key", "bitwarden-vault-key")

		e.GET("/data/"+consts.Settings+"/"+consts.BitwardenSettingsID).
			WithQuery("revs", "true").
			WithHeader("Authorization", "Bearer "+bitwardenToken).
			Expect().Status(200).
			JSON().Object().ValueEqual("key", "bitwarden-vault-key")
	})

	t.Run("GetForMissingDoc", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/data/no.such.doctype/id").
			Expect().Status(401)

		e.GET("/data/" + Type + "/no.such.id").
			Expect().Status(401)

		e.GET("/data/"+Type+"/no.such.id").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)
	})

	t.Run("GetWithSlash", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		err := couchdb.CreateNamedDoc(testInstance, &couchdb.JSONDoc{
			Type: Type, M: map[string]interface{}{
				"_id":  "with/slash",
				"test": "valueslash",
			}})
		require.NoError(t, err)

		e.GET("/data/"+Type+"/with%2Fslash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object().
			ValueEqual("test", "valueslash")
	})

	t.Run("WrongDoctype", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_ = couchdb.DeleteDB(testInstance, "io.cozy.nottype")

		e.GET("/data/io.cozy.nottype/"+ID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404).
			JSON().Object().
			ValueEqual("error", "not_found").
			ValueEqual("reason", "wrong_doctype")
	})

	t.Run("UnderscoreName", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/data/"+Type+"/_foo").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400)
	})

	t.Run("VFSDoctype", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/data/io.cozy.files/").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "wrong-vfs": "structure" }`)).
			Expect().Status(403).
			JSON().Object().
			Value("error").String().Contains("reserved")
	})

	t.Run("WrongID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/data/"+Type+"/NOTID").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404).
			JSON().Object().
			ValueEqual("error", "not_found").
			ValueEqual("reason", "missing")
	})

	t.Run("SuccessCreateKnownDoctype", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/data/"+Type+"/").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "somefield": "avalue" }`)).
			Expect().Status(201).
			JSON().Object()

		obj.ValueEqual("ok", true)
		obj.Value("id").String().NotEmpty()
		obj.ValueEqual("type", Type)
		obj.Value("rev").String().NotEmpty()

		var data couchdb.JSONDoc

		obj.Value("data").Decode(&data)
		obj.ValueEqual("id", data.ID())
		obj.ValueEqual("type", data.Type)
		obj.ValueEqual("rev", data.Rev())
		assert.Equal(t, "avalue", data.Get("somefield"))
	})

	t.Run("SuccessCreateUnknownDoctype", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		type2 := "io.cozy.anothertype"
		obj := e.POST("/data/"+type2+"/").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "somefield": "avalue" }`)).
			Expect().Status(201).
			JSON().Object()

		obj.ValueEqual("ok", true)
		obj.Value("id").String().NotEmpty()
		obj.ValueEqual("type", type2)
		obj.Value("rev").String().NotEmpty()

		var data couchdb.JSONDoc

		obj.Value("data").Decode(&data)
		obj.ValueEqual("id", data.ID())
		obj.ValueEqual("type", data.Type)
		obj.ValueEqual("rev", data.Rev())
		assert.Equal(t, "avalue", data.Get("somefield"))
	})

	t.Run("WrongCreateWithID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/data/"+Type+"/").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ 
        "_id": "this-should-not-be-an-id",
        "somefield": "avalue"
      }`)).
			Expect().Status(400)
	})

	t.Run("SuccessUpdate", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get revision
		doc := getDocForTest(Type, testInstance)

		// update it
		obj := e.PUT("/data/"+doc.DocType()+"/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithJSON(map[string]interface{}{
				"_id":       doc.ID(),
				"_rev":      doc.Rev(),
				"test":      doc.Get("test"),
				"somefield": "anewvalue",
			}).
			Expect().Status(200).
			JSON().Object()

		obj.NotContainsKey("error")
		obj.ValueEqual("id", doc.ID())
		obj.ValueEqual("ok", true)
		obj.Value("rev").String().NotEmpty()
		obj.ValueNotEqual("rev", doc.Rev())

		var data couchdb.JSONDoc

		obj.Value("data").Decode(&data)
		obj.ValueEqual("id", data.ID())
		obj.ValueEqual("type", data.Type)
		obj.ValueEqual("rev", data.Rev())
		assert.Equal(t, "anewvalue", data.Get("somefield"))
	})

	t.Run("WrongIDInDocUpdate", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get revision
		doc := getDocForTest(Type, testInstance)

		// update it
		e.PUT("/data/"+doc.DocType()+"/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithJSON(map[string]interface{}{
				"_id":       "this is not the id in the URL",
				"_rev":      doc.Rev(),
				"test":      doc.M["test"],
				"somefield": "anewvalue",
			}).
			Expect().Status(400)
	})

	t.Run("CreateDocWithAFixedID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// update it
		obj := e.PUT("/data/"+Type+"/specific-id").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithJSON(map[string]interface{}{
				"test":      "value",
				"somefield": "anewvalue",
			}).
			Expect().Status(200).
			JSON().Object()

		obj.NotContainsKey("error")
		obj.ValueEqual("id", "specific-id")
		obj.ValueEqual("ok", true)
		obj.Value("rev").String().NotEmpty()

		var data couchdb.JSONDoc
		obj.Value("data").Decode(&data)
		obj.ValueEqual("id", data.ID())
		obj.ValueEqual("type", data.Type)
		obj.ValueEqual("rev", data.Rev())
		assert.Equal(t, "anewvalue", data.Get("somefield"))
	})

	t.Run("NoRevInDocUpdate", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get revision
		doc := getDocForTest(Type, testInstance)

		// update it
		e.PUT("/data/"+doc.DocType()+"/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithJSON(map[string]interface{}{
				"_id":       doc.ID(),
				"test":      doc.M["test"],
				"somefield": "anewvalue",
			}).
			Expect().Status(400)
	})

	t.Run("PreviousRevInDocUpdate", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get revision
		doc := getDocForTest(Type, testInstance)
		firstRev := doc.Rev()

		// correcly update it
		e.PUT("/data/"+doc.DocType()+"/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithJSON(map[string]interface{}{
				"_id":       doc.ID(),
				"_rev":      doc.Rev(),
				"somefield": "anewvalue",
			}).
			Expect().Status(200)

		// update it
		e.PUT("/data/"+doc.DocType()+"/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithJSON(map[string]interface{}{
				"_id":       doc.ID(),
				"_rev":      firstRev,
				"somefield": "anewvalue2",
			}).
			Expect().Status(409)
	})

	t.Run("SuccessDeleteIfMatch", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get revision
		doc := getDocForTest(Type, testInstance)
		rev := doc.Rev()

		// Do deletion
		obj := e.DELETE("/data/"+doc.DocType()+"/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("If-Match", rev).
			Expect().Status(200).
			JSON().Object()

		obj.ValueEqual("id", doc.ID())
		obj.ValueEqual("ok", true)
		obj.ValueEqual("deleted", true)
		obj.ValueNotEqual("rev", doc.Rev())
	})

	t.Run("FailDeleteIfNotMatch", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get revision
		doc := getDocForTest(Type, testInstance)

		// Do deletion
		e.DELETE("/data/"+doc.DocType()+"/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("If-Match", "1-238238232322121"). // invalid rev
			Expect().Status(409)
	})

	t.Run("FailDeleteIfHeaderAndRevMismatch", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get revision
		doc := getDocForTest(Type, testInstance)

		// Do deletion
		e.DELETE("/data/"+doc.DocType()+"/"+doc.ID()).
			WithQuery("rev", "1-238238232322121").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("If-Match", "1-23823823231"). // not the same rev
			Expect().Status(400)
	})

	t.Run("FailDeleteIfNoRev", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get revision
		doc := getDocForTest(Type, testInstance)

		// Do deletion
		e.DELETE("/data/"+doc.DocType()+"/"+doc.ID()).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400)
	})

	t.Run("DefineIndex", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/data/"+Type+"/_index").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "index": { "fields": ["foo"] } }`)).
			Expect().Status(200).
			JSON().Object()

		obj.NotContainsKey("error")
		obj.NotContainsKey("reason")
		obj.ValueEqual("result", "created")
		obj.Value("name").String().NotEmpty()
		obj.Value("id").String().NotEmpty()
	})

	t.Run("ReDefineIndex", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/data/"+Type+"/_index").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "index": { "fields": ["foo"] } }`)).
			Expect().Status(200).
			JSON().Object()

		obj.NotContainsKey("error")
		obj.NotContainsKey("reason")
		obj.ValueEqual("result", "exists")
		obj.Value("name").String().NotEmpty()
		obj.Value("id").String().NotEmpty()
	})

	t.Run("DefineIndexUnexistingDoctype", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_ = couchdb.DeleteDB(testInstance, "io.cozy.nottype")

		obj := e.POST("/data/io.cozy.nottype/_index").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "index": { "fields": ["foo"] } }`)).
			Expect().Status(200).
			JSON().Object()

		obj.NotContainsKey("error")
		obj.NotContainsKey("reason")
		obj.ValueEqual("result", "created")
		obj.Value("name").String().NotEmpty()
		obj.Value("id").String().NotEmpty()
	})

	t.Run("FindDocuments", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_ = couchdb.ResetDB(testInstance, Type)

		// Insert some docs
		_ = getDocForTest(Type, testInstance)
		_ = getDocForTest(Type, testInstance)
		_ = getDocForTest(Type, testInstance)

		// Create and wait for the index used by the selector.
		indexName := e.POST("/data/"+Type+"/_index").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "index": { "fields": ["test"] } }`)).
			Expect().Status(200).
			JSON().Object().
			NotContainsKey("error").
			Value("name").String().Raw()

		require.Eventually(t, func() bool {
			var results []couchdb.JSONDoc
			err := couchdb.FindDocs(testInstance, Type, &couchdb.FindRequest{
				Selector: mango.Equal("test", "value"),
				UseIndex: indexName,
			}, &results)
			return err == nil
		}, 5*time.Second, 100*time.Millisecond, "index %q should be ready within 5s", indexName)

		obj := e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "test": "value" } }`)).
			Expect().Status(200).
			JSON().Object()

		docs := obj.Value("docs").Array()
		docs.Length().Equal(3)
		obj.NotContainsKey("execution_stats")
	})

	t.Run("FindDocumentsWithStats", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_ = couchdb.ResetDB(testInstance, Type)
		_ = getDocForTest(Type, testInstance)

		// Create the index
		indexName := e.POST("/data/"+Type+"/_index").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "index": { "fields": ["test"] } }`)).
			Expect().Status(200).
			JSON().Object().
			NotContainsKey("error").
			Value("name").String().Raw()

		// CouchDB builds indexes asynchronously. Wait until the index is ready
		// to serve queries before asserting on execution_stats.
		require.Eventually(t, func() bool {
			var results []couchdb.JSONDoc
			err := couchdb.FindDocs(testInstance, Type, &couchdb.FindRequest{
				Selector: mango.Equal("test", "value"),
				UseIndex: indexName,
			}, &results)
			return err == nil
		}, 5*time.Second, 100*time.Millisecond, "index %q should be ready within 5s", indexName)

		// Select with the index
		e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "test": "value" }, "execution_stats": true }`)).
			Expect().Status(200).
			JSON().Object().
			Value("execution_stats").Object().NotEmpty()
	})

	t.Run("FindDocumentsPaginated", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_ = couchdb.ResetDB(testInstance, Type)

		// Push 150 docs
		for i := 1; i <= 150; i++ {
			_ = getDocForTest(Type, testInstance)
		}

		// Create an index
		e.POST("/data/"+Type+"/_index").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "index": { "fields": ["test"] } }`)).
			Expect().Status(200).
			JSON().Object().
			NotContainsKey("error")

		// Select with the index
		obj := e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "test": "value" } }`)).
			Expect().Status(200).
			JSON().Object()

		docs := obj.Value("docs").Array()
		docs.Length().Equal(100)
		obj.ValueEqual("next", true)

		// A new select with the index and a limit
		obj = e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "test": "value" }, "limit": 10 }`)).
			Expect().Status(200).
			JSON().Object()

		docs = obj.Value("docs").Array()
		docs.Length().Equal(10)
		obj.ValueEqual("next", true)
	})

	t.Run("FindDocumentsPaginatedBookmark", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		_ = couchdb.ResetDB(testInstance, Type)

		// Insert 200 docs
		for i := 1; i <= 200; i++ {
			_ = getDocForTest(Type, testInstance)
		}

		// Create an index
		e.POST("/data/"+Type+"/_index").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "index": { "fields": ["test"] } }`)).
			Expect().Status(200).
			JSON().Object().
			NotContainsKey("error")

		// Select with the index
		obj := e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "test": "value" } }`)).
			Expect().Status(200).
			JSON().Object()

		docs := obj.Value("docs").Array()
		docs.Length().Equal(100)
		obj.ValueEqual("limit", 100)
		obj.ValueEqual("next", true)
		bm := obj.Value("bookmark").String().NotEmpty().Raw()

		// New select with the index and a bookmark
		obj = e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "test": "value"}, "bookmark": "` + bm + `" }`)).
			Expect().Status(200).
			JSON().Object()

		docs = obj.Value("docs").Array()
		docs.Length().Equal(100)
		obj.ValueEqual("limit", 100)
		obj.ValueEqual("next", true)
		bm = obj.Value("bookmark").String().NotEmpty().Raw()

		// Select 3 with the index and the same bookmark
		obj = e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "test": "value"}, "bookmark": "` + bm + `" }`)).
			Expect().Status(200).
			JSON().Object()

		docs = obj.Value("docs").Array()
		docs.Length().Equal(0)
		obj.ValueEqual("next", false)

		// Select 4 with the index a value matching nothing
		obj = e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "test": "novalue" }}`)).
			Expect().Status(200).
			JSON().Object()

		docs = obj.Value("docs").Array()
		docs.Length().Equal(0)
		obj.Value("bookmark").String().Empty()
	})

	t.Run("FindDocumentsWithoutIndex", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/data/"+Type+"/_find").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{ "selector": { "no-index-for-this-field": "value" } }`)).
			Expect().Status(400).
			JSON().Object()

		obj.Value("error").String().Contains("no_inde")
		obj.Value("reason").String().Contains("no matching index")
	})

	t.Run("GetChanges", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		assert.NoError(t, couchdb.ResetDB(testInstance, Type))

		seqno := e.GET("/data/"+Type+"/_changes").
			WithQuery("style", "all_docs").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object().
			Value("last_seq").String().NotEmpty().Raw()

		// creates 3 docs
		_ = getDocForTest(Type, testInstance)
		_ = getDocForTest(Type, testInstance)
		_ = getDocForTest(Type, testInstance)

		e.GET("/data/"+Type+"/_changes").
			WithQuery("limit", 2).
			WithQuery("since", seqno).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object().
			Value("results").Array().Length().Equal(2)

		e.GET("/data/"+Type+"/_changes").
			WithQuery("since", seqno).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object().
			Value("results").Array().Length().Equal(3)
	})

	t.Run("PostChanges", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		assert.NoError(t, couchdb.ResetDB(testInstance, Type))

		// creates 3 docs
		doc1 := getDocForTest(Type, testInstance)
		_ = getDocForTest(Type, testInstance)
		doc2 := getDocForTest(Type, testInstance)

		e.POST("/data/"+Type+"/_changes").
			WithQuery("include_docs", true).
			WithQuery("filter", "_doc_ids").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{"doc_ids": ["%s", "%s"]}`, doc1.ID(), doc2.ID()))).
			Expect().Status(200).
			JSON().Object().
			Value("results").Array().Length().Equal(2)
	})

	t.Run("WrongFeedChanges", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/data/"+Type+"/_changes").
			WithQuery("feed", "continuous").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400)
	})

	t.Run("WrongStyleChanges", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/data/"+Type+"/_changes").
			WithQuery("style", "not_a_valid_style").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400)
	})

	t.Run("LimitIsNoNumber", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/data/"+Type+"/_changes").
			WithQuery("limit", "not_a_number").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400)
	})

	t.Run("UnsupportedOption", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/data/"+Type+"/_changes").
			WithQuery("inlude_docs", true). // typo (inlude instead of include)
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400)
	})

	t.Run("GetAllDocs", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.GET("/data/"+Type+"/_all_docs").
			WithQuery("include_docs", true).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.ValueEqual("total_rows", 3)
		obj.ValueEqual("offset", 0)
		rows := obj.Value("rows").Array()
		rows.Length().Equal(3)

		first := rows.First().Object()
		first.Value("id").String().NotEmpty()
		doc := first.Value("doc").Object()
		doc.ValueEqual("test", "value")
		doc.Path("$.foo.bar").Equal("one")
		doc.Path("$.foo.baz").Equal("two")
		doc.Path("$.foo.qux").Equal("quux")
	})

	t.Run("GetAllDocsWithFields", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.GET("/data/"+Type+"/_all_docs").
			WithQuery("include_docs", true).
			WithQuery("Fields", "test,nosuchfield,foo.qux").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.ValueEqual("total_rows", 3)
		obj.ValueEqual("offset", 0)
		rows := obj.Value("rows").Array()
		rows.Length().Equal(3)

		first := rows.First().Object()
		first.Value("id").String().NotEmpty()
		doc := first.Value("doc").Object()
		doc.ValueEqual("test", "value")
		foo := doc.Value("foo").Object()
		foo.NotContainsKey("bar")
		foo.NotContainsKey("baz")
		foo.ValueEqual("qux", "quux")
	})

	t.Run("NormalDocs", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		view := &couchdb.View{
			Name:    "foobar",
			Doctype: Type,
			Map: `
	 function(doc) {
	   emit(doc.foobar, doc);
	 }`,
		}
		g, _ := errgroup.WithContext(context.Background())
		couchdb.DefineViews(g, testInstance, []*couchdb.View{view})
		require.NoError(t, g.Wait())

		err := couchdb.CreateNamedDoc(testInstance, &couchdb.JSONDoc{
			Type: Type,
			M: map[string]interface{}{
				"_id":  "four",
				"test": "fourthvalue",
			},
		})
		require.NoError(t, err)

		obj := e.GET("/data/"+Type+"/_normal_docs").
			WithQuery("limit", 2).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.ValueEqual("total_rows", 4)
		obj.Value("bookmark").String().NotEmpty()
		bookmark := obj.Value("bookmark").String().NotEmpty().Raw()
		obj.NotContainsKey("execution_stats")

		rows := obj.Value("rows").Array()
		rows.Length().Equal(2)

		elem := rows.Element(1).Object()
		elem.Value("_id").String().NotEmpty()
		elem.ValueEqual("test", "value")

		// skip pagination
		obj = e.GET("/data/"+Type+"/_normal_docs").
			WithQuery("limit", 2).
			WithQuery("skip", 2).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.ValueEqual("total_rows", 4)
		obj.Value("bookmark").String().NotEmpty()
		rows = obj.Value("rows").Array()
		rows.Length().Equal(2)

		elem = rows.Element(1).Object()
		elem.Value("_id").String().NotEmpty()
		elem.ValueEqual("test", "fourthvalue")

		// bookmark pagination
		obj = e.GET("/data/"+Type+"/_normal_docs").
			WithQuery("bookmark", bookmark).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.ValueEqual("total_rows", 4)
		rows = obj.Value("rows").Array()
		rows.Length().Equal(2)

		elem = rows.Element(1).Object()
		elem.Value("_id").String().NotEmpty()
		elem.ValueEqual("test", "fourthvalue")

		// _normal_docs with no results
		emptyType := "io.cozy.anothertype"
		_ = couchdb.ResetDB(testInstance, emptyType)

		obj = e.GET("/data/"+emptyType+"/_normal_docs").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.ValueEqual("total_rows", 0)
		obj.Value("bookmark").String().Empty()

		// execution stats
		obj = e.GET("/data/"+emptyType+"/_normal_docs").
			WithQuery("execution_stats", true).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.Value("execution_stats").Object().NotEmpty()
	})

	t.Run("GetDesignDocs", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		def := M{"index": M{"fields": S{"foo"}}}
		_, err := couchdb.DefineIndexRaw(testInstance, Type, &def)
		require.NoError(t, err)

		obj := e.GET("/data/"+Type+"/_design_docs").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		rows := obj.Value("rows").Array()
		rows.Length().Gt(0)

		elem := rows.First().Object()
		elem.Value("id").String().NotEmpty()
		elem.Path("$.value.rev").String().NotEmpty()
	})

	t.Run("GetDesignDoc", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		ddoc := "myindex"
		def := M{"index": M{"fields": S{"foo"}}, "ddoc": ddoc}
		_, err := couchdb.DefineIndexRaw(testInstance, Type, &def)
		assert.NoError(t, err)

		obj := e.GET("/data/"+Type+"/_design/"+ddoc).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.ValueEqual("_id", "_design/"+ddoc)
		obj.Value("_rev").String().NotEmpty()
	})

	t.Run("DeleteDesignDoc", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		def := M{"index": M{"fields": S{"foo"}}}
		_, err := couchdb.DefineIndexRaw(testInstance, Type, &def)
		require.NoError(t, err)

		// Get the number of design document
		obj := e.GET("/data/"+Type+"/_design_docs").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		rows := obj.Value("rows").Array()
		nbDD := rows.Length().Gt(0).Raw()

		elem := rows.Element(0).Object()
		id := elem.Value("id").String().NotEmpty().Raw()

		ddoc := strings.Split(id, "/")[1]
		rev := elem.Path("$.value.rev").String().NotEmpty().Raw()

		// Delete the first design document
		e.DELETE("/data/"+Type+"/_design/"+ddoc).
			WithQuery("rev", rev).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Check that the number of design document have decreased
		obj = e.GET("/data/"+Type+"/_design_docs").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		rows = obj.Value("rows").Array()
		rows.Length().Lt(nbDD)
	})

	t.Run("CannotDeleteStackDesignDoc", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Fetch the dir-by-path and by-parent-type-name indexes rev
		obj := e.GET("/data/io.cozy.files/_design_docs").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		var indexRev, viewRev string

		for _, row := range obj.Value("rows").Array().Iter() {
			info := row.Object()
			if info.Value("id").String().Raw() == "_design/dir-by-path" {
				indexRev = info.Path("$.value.rev").String().NotEmpty().Raw()
			}
			if info.Value("id").String().Raw() == "_design/by-parent-type-name" {
				viewRev = info.Path("$.value.rev").String().NotEmpty().Raw()
			}
		}

		// Try to delete the dir-by-path index
		e.DELETE("/data/io.cozy.files/_design/dir-by-path").
			WithQuery("rev", indexRev).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(403)

		// Try to delete the dir-by-path index
		e.DELETE("/data/io.cozy.files/_design/by-parent-type-name").
			WithQuery("rev", viewRev).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(403)
	})

	t.Run("CopyDesignDoc", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		srcDdoc := "indextocopy"
		targetID := "_design/indexcopied"
		def := M{"index": M{"fields": S{"foo"}}, "ddoc": srcDdoc}
		_, err := couchdb.DefineIndexRaw(testInstance, Type, &def)
		assert.NoError(t, err)

		rev := e.GET("/data/"+Type+"/_design/"+srcDdoc).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object().
			Value("_rev").String().NotEmpty().Raw()

		e.POST("/data/"+Type+"/_design/"+srcDdoc+"/copy").
			WithQuery("rev", rev).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Destination", targetID).
			Expect().Status(201).
			JSON().Object().
			ValueEqual("id", targetID).
			ValueEqual("rev", rev)
	})

	t.Run("DeleteDatabase", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.DELETE("/data/"+Type+"/").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object().
			ValueEqual("deleted", true)
	})

	t.Run("DeleteDatabaseNoPermission", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		doctype := "io.cozy.forbidden"
		e.DELETE("/data/"+doctype+"/").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(403)
	})
}

func getDocForTest(t string, instance *instance.Instance) *couchdb.JSONDoc {
	doc := couchdb.JSONDoc{
		Type: t,
		M: map[string]interface{}{
			"test":   "value",
			"foo":    map[string]interface{}{"bar": "one", "baz": "two", "qux": "quux"},
			"courge": 1,
		},
	}
	_ = couchdb.CreateDoc(instance, &doc)
	return &doc
}
