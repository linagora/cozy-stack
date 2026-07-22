package couchdb

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const TestDoctype = "io.cozy.testobject"

var TestPrefix = prefixer.NewPrefixer(0, "test", "couchdb-tests")
var receivedEventsMutex sync.Mutex
var receivedEvents map[string]*realtime.Event

type testDoc struct {
	TestID  string `json:"_id,omitempty"`
	TestRev string `json:"_rev,omitempty"`
	Test    string `json:"test"`
	FieldA  string `json:"fieldA,omitempty"`
	FieldB  int    `json:"fieldB,omitempty"`
}

type testJSONDoc struct {
	JSONDoc
}

func (t *testJSONDoc) DocType() string {
	return TestDoctype
}

func TestCloneDocPreservesDocType(t *testing.T) {
	doc := &testJSONDoc{JSONDoc: JSONDoc{M: map[string]interface{}{}}}

	cloned := cloneDoc(doc)

	assert.Equal(t, TestDoctype, cloned.DocType())
}

func TestCouchdb(t *testing.T) {
	if testing.Short() {
		t.Skip("a couchdb is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)

	if _, err := CheckStatus(context.Background()); err != nil {
		require.NoError(t, err, "This test need couchdb to run.")
	}

	require.NoError(t, ResetDB(TestPrefix, TestDoctype))

	receivedEvents = make(map[string]*realtime.Event)
	eventChan := realtime.GetHub().Subscriber(TestPrefix)
	eventChan.Subscribe(TestDoctype)
	go func() {
		for ev := range eventChan.Channel {
			receivedEventsMutex.Lock()
			receivedEvents[ev.Verb+ev.Doc.ID()] = ev
			receivedEventsMutex.Unlock()
		}
	}()

	t.Cleanup(eventChan.Close)
	t.Cleanup(func() { _ = DeleteDB(TestPrefix, TestDoctype) })

	t.Run("Errors", func(t *testing.T) {
		err := Error{StatusCode: 404, Name: "not_found", Reason: "missing"}
		assert.Contains(t, err.Error(), "not_found")
		assert.Contains(t, err.Error(), "missing")
	})

	t.Run("CreateDoc", func(t *testing.T) {
		var err error

		doc := makeTestDoc()
		assert.Empty(t, doc.Rev(), doc.ID())

		// Create the document
		err = CreateDoc(TestPrefix, doc)
		assert.NoError(t, err)
		assert.NotEmpty(t, doc.Rev(), doc.ID())

		docType, id := doc.DocType(), doc.ID()
		assertGotEvent(t, realtime.EventCreate, doc.ID())

		// Fetch it and see if its match
		fetched := &testDoc{}
		err = GetDoc(TestPrefix, docType, id, fetched)
		assert.NoError(t, err)
		assert.Equal(t, doc.ID(), fetched.ID())
		assert.Equal(t, doc.Rev(), fetched.Rev())
		assert.Equal(t, "somevalue", fetched.Test)

		revBackup := fetched.Rev()

		// Update it
		updated := fetched
		updated.Test = "changedvalue"
		err = UpdateDoc(TestPrefix, updated)
		assert.NoError(t, err)
		assert.NotEqual(t, revBackup, updated.Rev())
		assert.Equal(t, "changedvalue", updated.Test)
		evt := assertGotEvent(t, realtime.EventUpdate, doc.ID())
		assert.NotNil(t, evt.OldDoc)
		assert.Equal(t, "somevalue", evt.OldDoc.(*testDoc).Test)
		assert.Equal(t, "changedvalue", evt.Doc.(*testDoc).Test)

		// Refetch it and see if its match
		fetched2 := &testDoc{}
		err = GetDoc(TestPrefix, docType, id, fetched2)
		assert.NoError(t, err)
		assert.Equal(t, doc.ID(), fetched2.ID())
		assert.Equal(t, updated.Rev(), fetched2.Rev())
		assert.Equal(t, "changedvalue", fetched2.Test)

		// Delete it
		err = DeleteDoc(TestPrefix, updated)
		assert.NoError(t, err)
		evt = assertGotEvent(t, realtime.EventDelete, doc.ID())
		assert.Equal(t, TestDoctype, evt.Doc.DocType())
		assert.Equal(t, TestDoctype, evt.OldDoc.DocType())

		fetched3 := &testDoc{}
		err = GetDoc(TestPrefix, docType, id, fetched3)
		assert.Error(t, err)
		coucherr, iscoucherr := err.(*Error)
		if assert.True(t, iscoucherr) {
			assert.Equal(t, coucherr.Reason, "deleted")
		}
	})

	t.Run("DeleteDoc preserves an embedded JSONDoc type", func(t *testing.T) {
		doc := &testJSONDoc{JSONDoc: JSONDoc{M: map[string]interface{}{
			"_id":  "embedded-json-doc",
			"test": "value",
		}}}
		require.NoError(t, CreateNamedDoc(TestPrefix, doc))
		assertGotEvent(t, realtime.EventCreate, doc.ID())

		require.NoError(t, DeleteDoc(TestPrefix, doc))
		evt := assertGotEvent(t, realtime.EventDelete, doc.ID())
		assert.Equal(t, TestDoctype, evt.Doc.DocType())
		assert.Equal(t, TestDoctype, evt.OldDoc.DocType())
	})

	t.Run("GetAllDocs", func(t *testing.T) {
		doc1 := &testDoc{Test: "all_1"}
		doc2 := &testDoc{Test: "all_2"}
		assert.NoError(t, CreateDoc(TestPrefix, doc1))
		assert.NoError(t, CreateDoc(TestPrefix, doc2))

		var results []*testDoc
		err := GetAllDocs(TestPrefix, TestDoctype, &AllDocsRequest{Limit: 2}, &results)
		if assert.NoError(t, err) {
			assert.Len(t, results, 2)
			assert.Equal(t, results[0].Test, "all_1")
			assert.Equal(t, results[1].Test, "all_2")
		}
	})

	t.Run("GetDocRevs", func(t *testing.T) {
		doc := &testDoc{Test: "1"}
		assert.NoError(t, CreateDoc(TestPrefix, doc))
		rev1 := doc.TestRev
		doc.Test = "2"
		assert.NoError(t, UpdateDoc(TestPrefix, doc))
		rev2 := doc.TestRev

		res := &JSONDoc{}
		err := GetDocWithRevs(TestPrefix, TestDoctype, doc.TestID, res)
		revisions := res.M["_revisions"].(map[string]interface{})
		ids := revisions["ids"].([]interface{})
		assert.NoError(t, err)
		assert.Len(t, ids, 2)
		sliceRev1 := strings.SplitN(rev1, "-", 2)
		assert.Len(t, sliceRev1, 2)
		sliceRev2 := strings.SplitN(rev2, "-", 2)
		assert.Len(t, sliceRev2, 2)

		assert.Equal(t, ids[0].(string), sliceRev2[1])
		assert.Equal(t, ids[1].(string), sliceRev1[1])
	})

	t.Run("BulkUpdateDocs", func(t *testing.T) {
		doc1 := &testDoc{Test: "before_1"}
		doc2 := &testDoc{Test: "before_2"}
		assert.NoError(t, CreateDoc(TestPrefix, doc1))
		assert.NoError(t, CreateDoc(TestPrefix, doc2))

		var results []*testDoc
		err := GetAllDocs(TestPrefix, TestDoctype, &AllDocsRequest{Limit: 2}, &results)
		assert.NoError(t, err)
		results[0].Test = "after_1"
		results[1].Test = "after_2"

		olddocs := make([]interface{}, len(results))
		docs := make([]interface{}, len(results))
		for i, doc := range results {
			docs[i] = doc
		}
		err = BulkUpdateDocs(TestPrefix, results[0].DocType(), docs, olddocs)
		assert.NoError(t, err)

		err = GetAllDocs(TestPrefix, TestDoctype, &AllDocsRequest{Limit: 2}, &results)
		if assert.NoError(t, err) {
			assert.Len(t, results, 2)
			assert.Equal(t, results[0].Test, "after_1")
			assert.Equal(t, results[1].Test, "after_2")
		}
	})

	t.Run("DefineIndex", func(t *testing.T) {
		err := DefineIndex(TestPrefix, mango.MakeIndex(TestDoctype, "my-index", mango.IndexDef{Fields: []string{"fieldA", "fieldB"}}))
		assert.NoError(t, err)

		// if I try to define the same index several time
		err2 := DefineIndex(TestPrefix, mango.MakeIndex(TestDoctype, "my-index", mango.IndexDef{Fields: []string{"fieldA", "fieldB"}}))
		assert.NoError(t, err2)
	})

	t.Run("DefineIndexWithPartialFilter", func(t *testing.T) {
		err := DefineIndex(TestPrefix, mango.MakeIndex(TestDoctype, "my-index-with-partial-filter", mango.IndexDef{Fields: []string{"fieldA"}, PartialFilter: mango.NotExists("fieldB")}))
		assert.NoError(t, err)

		// if I try to define the same index several time
		err2 := DefineIndex(TestPrefix, mango.MakeIndex(TestDoctype, "my-index-with-partial-filter", mango.IndexDef{Fields: []string{"fieldA"}, PartialFilter: mango.NotExists("fieldB")}))
		assert.NoError(t, err2)
	})

	t.Run("Query", func(t *testing.T) {
		// create a few docs for testing
		doc1 := testDoc{FieldA: "value1", FieldB: 100}
		doc2 := testDoc{FieldA: "value2", FieldB: 1000}
		doc3 := testDoc{FieldA: "value2", FieldB: 300}
		doc4 := testDoc{FieldA: "value1", FieldB: 1500}
		doc5 := testDoc{FieldA: "value1", FieldB: 150}
		docs := []*testDoc{&doc1, &doc2, &doc3, &doc4, &doc5}
		for _, doc := range docs {
			err := CreateDoc(TestPrefix, doc)
			if !assert.NoError(t, err) || doc.ID() == "" {
				t.FailNow()
				return
			}
		}

		err := DefineIndex(TestPrefix, mango.MakeIndex(TestDoctype, "my-index", mango.IndexDef{Fields: []string{"fieldA", "fieldB"}}))
		if !assert.NoError(t, err) {
			t.FailNow()
			return
		}
		var out []testDoc
		req := &FindRequest{
			UseIndex: "my-index",
			Selector: mango.And(
				mango.Equal("fieldA", "value2"),
				mango.Exists("fieldB"),
			),
		}
		err = FindDocs(TestPrefix, TestDoctype, req, &out)
		if assert.NoError(t, err) {
			assert.Len(t, out, 2, "should get 2 results")
			// if fieldA are equaly, docs will be ordered by fieldB
			assert.Equal(t, doc3.ID(), out[0].ID())
			assert.Equal(t, "value2", out[0].FieldA)
			assert.Equal(t, doc2.ID(), out[1].ID())
			assert.Equal(t, "value2", out[1].FieldA)
		}

		var out2 []testDoc
		req2 := &FindRequest{
			UseIndex: "my-index",
			Selector: mango.And(
				mango.Equal("fieldA", "value1"),
				mango.Between("fieldB", 10, 1000),
			),
		}
		err = FindDocs(TestPrefix, TestDoctype, req2, &out2)
		if assert.NoError(t, err) {
			assert.Len(t, out, 2, "should get 2 results")
			assert.Equal(t, doc1.ID(), out2[0].ID())
			assert.Equal(t, doc5.ID(), out2[1].ID())
		}
	})

	t.Run("ForeachDocs", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			doc := &testDoc{Test: fmt.Sprintf("foreach_%d", i)}
			require.NoError(t, CreateDoc(TestPrefix, doc))
		}

		var results []*testDoc
		err := GetAllDocs(TestPrefix, TestDoctype, &AllDocsRequest{}, &results)
		require.NoError(t, err)
		var expected []string
		for _, result := range results {
			expected = append(expected, result.Test)
		}

		var keys []string
		ForeachDocsWithCustomPagination(TestPrefix, TestDoctype, 2, func(id string, raw json.RawMessage) error {
			var doc testDoc
			err := json.Unmarshal(raw, &doc)
			if err != nil {
				return err
			}
			keys = append(keys, doc.Test)
			return nil
		})

		assert.Equal(t, expected, keys)
	})

	t.Run("ChangesSuccess", func(t *testing.T) {
		err := ResetDB(TestPrefix, TestDoctype)
		assert.NoError(t, err)

		request := &ChangesRequest{
			DocType: TestDoctype,
		}
		response, err := GetChanges(TestPrefix, request)
		seqnoAfterCreates := response.LastSeq
		assert.NoError(t, err)
		assert.Len(t, response.Results, 0)

		doc1 := makeTestDoc()
		doc2 := makeTestDoc()
		doc3 := makeTestDoc()
		assert.NoError(t, CreateDoc(TestPrefix, doc1))
		assert.NoError(t, CreateDoc(TestPrefix, doc2))
		assert.NoError(t, CreateDoc(TestPrefix, doc3))

		request = &ChangesRequest{
			DocType: TestDoctype,
			Since:   seqnoAfterCreates,
		}

		response, err = GetChanges(TestPrefix, request)
		assert.NoError(t, err)
		assert.Len(t, response.Results, 3)

		request = &ChangesRequest{
			DocType: TestDoctype,
			Since:   seqnoAfterCreates,
			Limit:   2,
		}

		response, err = GetChanges(TestPrefix, request)
		assert.NoError(t, err)
		assert.Len(t, response.Results, 2)

		seqnoAfterCreates = response.LastSeq

		doc4 := makeTestDoc()
		assert.NoError(t, CreateDoc(TestPrefix, doc4))

		request = &ChangesRequest{
			DocType: TestDoctype,
			Since:   seqnoAfterCreates,
		}
		response, err = GetChanges(TestPrefix, request)
		assert.NoError(t, err)
		assert.Len(t, response.Results, 2)
	})

	t.Run("EnsureDBExist", func(t *testing.T) {
		defer func() { _ = DeleteDB(TestPrefix, "io.cozy.tests.db1") }()
		_, err := DBStatus(TestPrefix, "io.cozy.tests.db1")
		assert.True(t, IsNoDatabaseError(err))
		assert.NoError(t, EnsureDBExist(TestPrefix, "io.cozy.tests.db1"))
		_, err = DBStatus(TestPrefix, "io.cozy.tests.db1")
		assert.NoError(t, err)
	})

	t.Run("UpdateJSONDoc", func(t *testing.T) {
		var err error

		doc := &JSONDoc{
			Type: TestDoctype,
			M: map[string]interface{}{
				"test": "1",
			},
		}
		assert.Empty(t, doc.Rev(), doc.ID())

		// Create the document
		err = CreateDoc(TestPrefix, doc)
		assert.NoError(t, err)
		assert.NotEmpty(t, doc.Rev(), doc.ID())
		assertGotEvent(t, realtime.EventCreate, doc.ID())

		// Update it
		updated := &JSONDoc{
			Type: TestDoctype,
			M: map[string]interface{}{
				"_id":  doc.ID(),
				"_rev": doc.Rev(),
				"test": "2",
			},
		}
		err = UpdateDoc(TestPrefix, updated)
		assert.NoError(t, err)
		assert.NotEqual(t, doc.Rev(), updated.Rev())
		assert.Equal(t, "2", updated.M["test"])
		evt := assertGotEvent(t, realtime.EventUpdate, doc.ID())
		assert.NotNil(t, evt)
		assert.NotNil(t, evt.OldDoc)
		assert.Equal(t, "1", evt.OldDoc.(*JSONDoc).M["test"])
		assert.Equal(t, "2", evt.Doc.(*JSONDoc).M["test"])

		// Remove the test field
		noTest := &JSONDoc{
			Type: TestDoctype,
			M: map[string]interface{}{
				"_id":  updated.ID(),
				"_rev": updated.Rev(),
				"foo":  "bar",
			},
		}
		err = UpdateDoc(TestPrefix, noTest)
		assert.NoError(t, err)
		assert.NotEqual(t, updated.Rev(), noTest.Rev())
		assert.Empty(t, noTest.M["test"])
		evt = assertGotEvent(t, realtime.EventUpdate, doc.ID())
		assert.NotNil(t, evt)
		assert.NotNil(t, evt.OldDoc)
		assert.Equal(t, "2", evt.OldDoc.(*JSONDoc).M["test"])
		assert.Empty(t, evt.Doc.(*JSONDoc).M["test"])

		// Add the test field
		withTest := &JSONDoc{
			Type: TestDoctype,
			M: map[string]interface{}{
				"_id":  noTest.ID(),
				"_rev": noTest.Rev(),
				"foo":  "baz",
				"test": "3",
			},
		}
		err = UpdateDoc(TestPrefix, withTest)
		assert.NoError(t, err)
		assert.NotEqual(t, noTest.Rev(), withTest.Rev())
		assert.Equal(t, "3", withTest.M["test"])
		evt = assertGotEvent(t, realtime.EventUpdate, doc.ID())
		assert.NotNil(t, evt)
		assert.NotNil(t, evt.OldDoc)
		assert.Empty(t, evt.OldDoc.(*JSONDoc).M["test"])
		assert.Equal(t, "3", evt.Doc.(*JSONDoc).M["test"])
	})

	t.Run("JSONDocClone", func(t *testing.T) {
		var m map[string]interface{}
		data := []byte(`{
	"foo1": "bar",
	"foo2": [0,1,2,3],
	"foo3": ["abc", 1, 1.1],
	"foo4": {
		"bar1":"bar",
		"bar2": [0,1,2,3],
		"bar3": ["abc", 1, 1.1, { "key": "value", "key2": [{}, 1, 2, 3] }],
		"bar4": {}
	},
	"foo5": 1,
	"foo6": 0.001,
	"foo7": "toto"
}`)

		err := json.Unmarshal(data, &m)
		assert.NoError(t, err)
		j1 := JSONDoc{
			Type: "toto",
			M:    m,
		}
		j2 := j1.Clone().(*JSONDoc)

		assert.Equal(t, j1.Type, j2.Type)
		assert.True(t, reflect.DeepEqual(j1.M, j2.M))

		assert.False(t, reflect.ValueOf(j1.M["foo2"]).Pointer() == reflect.ValueOf(j2.M["foo2"]).Pointer())
		assert.False(t, reflect.ValueOf(j1.M["foo3"]).Pointer() == reflect.ValueOf(j2.M["foo3"]).Pointer())
		assert.False(t, reflect.ValueOf(j1.M["foo4"]).Pointer() == reflect.ValueOf(j2.M["foo4"]).Pointer())

		s1 := j1.M["foo1"].(string)
		s2 := j2.M["foo1"].(string)
		s3 := j1.M["foo7"].(string)
		s4 := j2.M["foo7"].(string)

		hdr1 := (*reflect.StringHeader)(unsafe.Pointer(&s1))
		hdr2 := (*reflect.StringHeader)(unsafe.Pointer(&s2))
		hdr3 := (*reflect.StringHeader)(unsafe.Pointer(&s3))
		hdr4 := (*reflect.StringHeader)(unsafe.Pointer(&s4))

		assert.Equal(t, hdr1.Data, hdr2.Data)
		assert.Equal(t, hdr1.Len, hdr2.Len)

		assert.Equal(t, hdr3.Data, hdr4.Data)
		assert.Equal(t, hdr3.Len, hdr4.Len)

		assert.NotEqual(t, hdr1.Data, hdr4.Data)
		assert.NotEqual(t, hdr1.Len, hdr4.Len)
	})

	t.Run("LocalDocuments", func(t *testing.T) {
		id := "foo"
		_, err := GetLocal(TestPrefix, TestDoctype, id)
		assert.True(t, IsNotFoundError(err))

		doc := map[string]interface{}{"bar": "baz"}
		err = PutLocal(TestPrefix, TestDoctype, id, doc)
		assert.NoError(t, err)
		assert.NotEmpty(t, doc["_rev"])

		out, err := GetLocal(TestPrefix, TestDoctype, id)
		assert.NoError(t, err)
		assert.Equal(t, "baz", out["bar"])

		err = DeleteLocal(TestPrefix, TestDoctype, id)
		assert.NoError(t, err)

		_, err = GetLocal(TestPrefix, TestDoctype, id)
		assert.True(t, IsNotFoundError(err))
	})
}

func (t *testDoc) ID() string {
	return t.TestID
}

func (t *testDoc) Rev() string {
	return t.TestRev
}

func (t *testDoc) DocType() string {
	return TestDoctype
}

func (t *testDoc) Clone() Doc {
	cloned := *t
	return &cloned
}

func (t *testDoc) SetID(id string) {
	t.TestID = id
}

func (t *testDoc) SetRev(rev string) {
	t.TestRev = rev
}

func makeTestDoc() Doc {
	return &testDoc{
		Test: "somevalue",
	}
}

func assertGotEvent(t *testing.T, eventType, id string) *realtime.Event {
	t.Helper()

	var event *realtime.Event
	var ok bool

	for i := 0; i < 200; i++ {
		receivedEventsMutex.Lock()
		event, ok = receivedEvents[eventType+id]
		receivedEventsMutex.Unlock()
		if ok {
			delete(receivedEvents, eventType+id)
			break
		}
		time.Sleep(time.Millisecond)
	}

	assert.True(t, ok, "Expected event %s:%s", eventType, id)
	return event
}
