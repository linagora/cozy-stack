package jsonapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

func TestBindNullInput(t *testing.T) {
	for _, body := range []string{"null", `{"data":[null]}`} {
		_, err := BindCompound(strings.NewReader(body))
		assert.Equal(t, BadJSON(), err)
	}
	_, err := BindRelations(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("null")))
	assert.Equal(t, BadJSON(), err)
}

func TestJsonapi(t *testing.T) {
	config.UseTestFile(t)

	router := echo.New()
	router.GET("/foos/courge", func(c echo.Context) error {
		courge := &Foo{FID: "courge", FRev: "1-abc", Bar: "baz"}
		return Data(c, 200, courge, nil)
	})

	router.GET("/paginated", func(c echo.Context) error {
		cursor, err := ExtractPaginationCursor(c, 13, 1000)
		if err != nil {
			return err
		}

		if c2, ok := cursor.(*couchdb.SkipCursor); ok {
			return c.JSON(200, fmt.Sprintf("key %d %d", c2.Limit, c2.Skip))
		}

		if c3, ok := cursor.(*couchdb.StartKeyCursor); ok {
			return c.JSON(200, fmt.Sprintf("key %d %s %s", c3.Limit, c3.NextKey, c3.NextDocID))
		}

		return fmt.Errorf("Wrong cursor type")
	})

	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)

	t.Run("ObjectMarshalling", func(t *testing.T) {
		foo := &Foo{FID: "courge", FRev: "1-abc", Bar: "baz"}
		raw, err := MarshalObject(foo)
		assert.NoError(t, err)
		var data map[string]interface{}
		err = json.Unmarshal(raw, &data)

		assert.NoError(t, err)
		assert.Equal(t, data["type"], "io.cozy.foos")
		assert.Equal(t, data["id"], "courge")
		assert.Contains(t, data, "meta")
		meta, _ := data["meta"].(map[string]interface{})
		assert.Equal(t, meta["rev"], "1-abc")
		assert.Contains(t, data, "attributes")
		attrs, _ := data["attributes"].(map[string]interface{})
		assert.Equal(t, attrs["bar"], "baz")
		assert.Contains(t, data, "links")
		links, _ := data["links"].(map[string]interface{})
		assert.Equal(t, links["self"], "/foos/courge")

		assert.Contains(t, data, "relationships")
		rels, _ := data["relationships"].(map[string]interface{})
		assert.Contains(t, rels, "single")
		single, _ := rels["single"].(map[string]interface{})
		assert.Contains(t, single, "links")
		links1, _ := single["links"].(map[string]interface{})
		assert.Equal(t, links1["related"], "/foos/courge/single")
		assert.Contains(t, single, "data")
		data1, _ := single["data"].(map[string]interface{})
		assert.Equal(t, data1["type"], "io.cozy.foos")
		assert.Equal(t, data1["id"], "qux")

		assert.Contains(t, rels, "multiple")
		multiple, _ := rels["multiple"].(map[string]interface{})
		assert.Contains(t, multiple, "links")
		links2, _ := multiple["links"].(map[string]interface{})
		assert.Equal(t, links2["related"], "/foos/courge/multiple")
		assert.Contains(t, multiple, "data")
		data2, _ := multiple["data"].([]interface{})
		assert.Len(t, data2, 1)
		qux, _ := data2[0].(map[string]interface{})
		assert.Equal(t, qux["type"], "io.cozy.foos")
		assert.Equal(t, qux["id"], "qux")
	})

	t.Run("Data", func(t *testing.T) {
		res, err := http.Get(ts.URL + "/foos/courge")
		assert.NoError(t, err)
		assert.Equal(t, "200 OK", res.Status, "should get a 200")
		assert.Equal(t, "application/vnd.api+json", res.Header.Get("Content-Type"))
		defer res.Body.Close()
		var body map[string]interface{}
		assert.NoError(t, json.NewDecoder(res.Body).Decode(&body))

		assert.Contains(t, body, "data")
		data := body["data"].(map[string]interface{})
		assert.Equal(t, data["type"], "io.cozy.foos")
		assert.Equal(t, data["id"], "courge")
		assert.Contains(t, data, "attributes")
		assert.Contains(t, data, "relationships")
		assert.Contains(t, data, "links")

		assert.Contains(t, body, "included")
		included := body["included"].([]interface{})
		assert.Len(t, included, 1)
		qux, _ := included[0].(map[string]interface{})
		assert.Equal(t, qux["type"], "io.cozy.foos")
		assert.Equal(t, qux["id"], "qux")
	})

	t.Run("Pagination", func(t *testing.T) {
		res, err := http.Get(ts.URL + "/paginated")
		assert.NoError(t, err)
		defer res.Body.Close()
		var c string
		assert.NoError(t, json.NewDecoder(res.Body).Decode(&c))
		assert.Equal(t, "key 13 %!s(<nil>) ", c)
	})

	t.Run("PaginationCustomLimit", func(t *testing.T) {
		res, err := http.Get(ts.URL + "/paginated?page[limit]=7")
		assert.NoError(t, err)
		defer res.Body.Close()
		var c string
		assert.NoError(t, json.NewDecoder(res.Body).Decode(&c))
		assert.Equal(t, "key 7 %!s(<nil>) ", c)
	})

	t.Run("PaginationBadNumber", func(t *testing.T) {
		res, err := http.Get(ts.URL + "/paginated?page[limit]=notnumber")
		assert.NoError(t, err)
		defer res.Body.Close()
		assert.NotEqual(t, http.StatusOK, res.StatusCode, "should give an error")
	})

	t.Run("PaginationWithCursor", func(t *testing.T) {
		res, err := http.Get(ts.URL + "/paginated?page[cursor]=%5B%5B%22a%22%2C%20%22b%22%5D%2C%20%22c%22%5D")
		assert.NoError(t, err)
		defer res.Body.Close()
		var c string
		assert.NoError(t, json.NewDecoder(res.Body).Decode(&c))
		assert.Equal(t, "key 13 [a b] c", c)
	})
}

type Foo struct {
	FID  string `json:"-"`
	FRev string `json:"-"`
	Bar  string `json:"bar"`
}

func (f *Foo) ID() string {
	return f.FID
}

func (f *Foo) Rev() string {
	return f.FRev
}

func (f *Foo) DocType() string {
	return "io.cozy.foos"
}

func (f *Foo) Clone() couchdb.Doc {
	return f
}

func (f *Foo) SetID(id string) {
	f.FID = id
}

func (f *Foo) SetRev(rev string) {
	f.FRev = rev
}

func (f *Foo) Links() *LinksList {
	return &LinksList{Self: "/foos/" + f.FID}
}

func (f *Foo) Relationships() RelationshipMap {
	qux := map[string]string{
		"type": "io.cozy.foos",
		"id":   "qux",
	}
	single := Relationship{
		Links: &LinksList{
			Related: "/foos/" + f.FID + "/single",
		},
		Data: qux,
	}
	multiple := Relationship{
		Links: &LinksList{
			Related: "/foos/" + f.FID + "/multiple",
		},
		Data: []map[string]string{qux},
	}
	return RelationshipMap{
		"single":   single,
		"multiple": multiple,
	}
}

func (f *Foo) Included() []Object {
	qux := &Foo{FID: "qux", FRev: "42-xyz", Bar: "quux"}
	return []Object{qux}
}

func TestExtractPaginationCursorRejectsNonPositiveLimit(t *testing.T) {
	for _, limit := range []string{"0", "-1", "-2147483648"} {
		req := httptest.NewRequest(http.MethodGet, "/?page[limit]="+limit, nil)
		c := echo.New().NewContext(req, httptest.NewRecorder())
		cursor, err := ExtractPaginationCursor(c, 30, 100)
		assert.Nil(t, cursor, "page[limit]=%s", limit)
		var jerr *Error
		if assert.ErrorAs(t, err, &jerr, "page[limit]=%s", limit) {
			assert.Equal(t, http.StatusBadRequest, jerr.Status)
		}
	}
}
