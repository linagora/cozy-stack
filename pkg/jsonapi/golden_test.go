package jsonapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
)

// plain is a Foo without links, relationships nor included resources.
type plain struct{ Foo }

func (p *plain) Links() *LinksList              { return nil }
func (p *plain) Relationships() RelationshipMap { return nil }
func (p *plain) Included() []Object             { return nil }

func goldenOutputs(t testing.TB) map[string]string {
	out := map[string]string{}
	foo := &Foo{FID: "a<b", FRev: "1-abc", Bar: "<b>&\"é "}
	raw, err := MarshalObject(foo)
	if err != nil {
		t.Fatal(err)
	}
	out["object"] = string(raw)

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := Data(c, http.StatusOK, foo, &LinksList{Self: "/foos/a"}); err != nil {
		t.Fatal(err)
	}
	out["data"] = rec.Body.String()

	rec = httptest.NewRecorder()
	c = echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := Data(c, http.StatusOK, &plain{Foo{FID: "p", Bar: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	out["data-only"] = rec.Body.String()

	list := func(name string, objs []Object, links *LinksList) {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		if err := DataList(c, http.StatusOK, objs, links); err != nil {
			t.Fatal(err)
		}
		out[name] = rec.Body.String()
	}
	list("list", []Object{foo, &plain{Foo{FID: "p", Bar: "x"}}}, &LinksList{Next: "/foos?page[skip]=2"})
	list("empty", nil, nil)

	rec = httptest.NewRecorder()
	c = echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := DataListWithIncluded(c, http.StatusOK, []Object{foo}, nil); err != nil {
		t.Fatal(err)
	}
	out["included"] = rec.Body.String()
	return out
}

// TestGoldenBytes pins the exact bytes of the JSON:API responses: key order,
// HTML escaping, omitted members and the trailing newline.
func TestGoldenBytes(t *testing.T) {
	want := map[string]string{
		"empty":     "{\"data\":[],\"meta\":{\"count\":0}}\n",
		"data-only": "{\"data\":{\"type\":\"io.cozy.foos\",\"id\":\"p\",\"attributes\":{\"bar\":\"x\"},\"meta\":{}}}\n",
		"included":  "{\"data\":[{\"type\":\"io.cozy.foos\",\"id\":\"a\\u003cb\",\"attributes\":{\"bar\":\"\\u003cb\\u003e\\u0026\\\"é\\u2028\"},\"meta\":{\"rev\":\"1-abc\"},\"links\":{\"self\":\"/foos/a\\u003cb\"},\"relationships\":{\"multiple\":{\"links\":{\"related\":\"/foos/a\\u003cb/multiple\"},\"data\":[{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}]},\"single\":{\"links\":{\"related\":\"/foos/a\\u003cb/single\"},\"data\":{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}}}}],\"meta\":{\"count\":1},\"included\":[{\"type\":\"io.cozy.foos\",\"id\":\"qux\",\"attributes\":{\"bar\":\"quux\"},\"meta\":{\"rev\":\"42-xyz\"},\"links\":{\"self\":\"/foos/qux\"},\"relationships\":{\"multiple\":{\"links\":{\"related\":\"/foos/qux/multiple\"},\"data\":[{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}]},\"single\":{\"links\":{\"related\":\"/foos/qux/single\"},\"data\":{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}}}}]}\n",
		"object":    "{\"type\":\"io.cozy.foos\",\"id\":\"a\\u003cb\",\"attributes\":{\"bar\":\"\\u003cb\\u003e\\u0026\\\"é\\u2028\"},\"meta\":{\"rev\":\"1-abc\"},\"links\":{\"self\":\"/foos/a\\u003cb\"},\"relationships\":{\"multiple\":{\"links\":{\"related\":\"/foos/a\\u003cb/multiple\"},\"data\":[{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}]},\"single\":{\"links\":{\"related\":\"/foos/a\\u003cb/single\"},\"data\":{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}}}}",
		"data":      "{\"data\":{\"type\":\"io.cozy.foos\",\"id\":\"a\\u003cb\",\"attributes\":{\"bar\":\"\\u003cb\\u003e\\u0026\\\"é\\u2028\"},\"meta\":{\"rev\":\"1-abc\"},\"links\":{\"self\":\"/foos/a\\u003cb\"},\"relationships\":{\"multiple\":{\"links\":{\"related\":\"/foos/a\\u003cb/multiple\"},\"data\":[{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}]},\"single\":{\"links\":{\"related\":\"/foos/a\\u003cb/single\"},\"data\":{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}}}},\"links\":{\"self\":\"/foos/a\"},\"included\":[{\"type\":\"io.cozy.foos\",\"id\":\"qux\",\"attributes\":{\"bar\":\"quux\"},\"meta\":{\"rev\":\"42-xyz\"},\"links\":{\"self\":\"/foos/qux\"},\"relationships\":{\"multiple\":{\"links\":{\"related\":\"/foos/qux/multiple\"},\"data\":[{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}]},\"single\":{\"links\":{\"related\":\"/foos/qux/single\"},\"data\":{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}}}}]}\n",
		"list":      "{\"data\":[{\"type\":\"io.cozy.foos\",\"id\":\"a\\u003cb\",\"attributes\":{\"bar\":\"\\u003cb\\u003e\\u0026\\\"é\\u2028\"},\"meta\":{\"rev\":\"1-abc\"},\"links\":{\"self\":\"/foos/a\\u003cb\"},\"relationships\":{\"multiple\":{\"links\":{\"related\":\"/foos/a\\u003cb/multiple\"},\"data\":[{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}]},\"single\":{\"links\":{\"related\":\"/foos/a\\u003cb/single\"},\"data\":{\"id\":\"qux\",\"type\":\"io.cozy.foos\"}}}},{\"type\":\"io.cozy.foos\",\"id\":\"p\",\"attributes\":{\"bar\":\"x\"},\"meta\":{}}],\"links\":{\"next\":\"/foos?page[skip]=2\"},\"meta\":{\"count\":2}}\n",
	}
	assert.Equal(t, want, goldenOutputs(t))
}

func BenchmarkDataList(b *testing.B) {
	objs := make([]Object, 100)
	for i := range objs {
		doc := couchdb.JSONDoc{Type: "io.cozy.files", M: map[string]interface{}{
			"_id": fmt.Sprintf("%032d", i), "_rev": "3-0123456789abcdef", "type": "file",
			"name": fmt.Sprintf("IMG_%04d.jpg", i), "dir_id": "io.cozy.files.root-dir",
			"created_at": "2026-11-27T12:00:00Z", "updated_at": "2026-11-27T12:00:00Z",
			"size": "2345678", "md5sum": "rL0Y20zC+Fzt72VPzMSk2A==", "mime": "image/jpeg", "class": "image",
			"metadata": map[string]interface{}{"datetime": "2026-11-27T12:00:00Z", "height": 3024, "width": 4032,
				"gps": map[string]interface{}{"lat": 48.85, "long": 2.35}},
			"cozyMetadata": map[string]interface{}{"doctypeVersion": "1", "metadataVersion": 1,
				"createdAt": "2026-11-27T12:00:00Z", "uploadedOn": "https://alice.example.net/"},
		}}
		objs[i] = &benchFile{doc}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		if err := DataList(c, http.StatusOK, objs, nil); err != nil {
			b.Fatal(err)
		}
	}
}

type benchFile struct{ couchdb.JSONDoc }

func (f *benchFile) Links() *LinksList              { return &LinksList{Self: "/files/" + f.ID()} }
func (f *benchFile) Relationships() RelationshipMap { return nil }
func (f *benchFile) Included() []Object             { return nil }
