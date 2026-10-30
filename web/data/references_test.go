package data

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReferences(t *testing.T) {
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

	t.Run("ListReferencesHandler", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Make doc
		doc := getDocForTest(Type, testInstance)

		// Make Files
		makeReferencedTestFile(t, testInstance, doc, "testtoref2.txt")
		makeReferencedTestFile(t, testInstance, doc, "testtoref3.txt")
		makeReferencedTestFile(t, testInstance, doc, "testtoref4.txt")
		makeReferencedTestFile(t, testInstance, doc, "testtoref5.txt")

		// Simple query
		obj := e.GET("/data/"+doc.DocType()+"/"+doc.ID()+"/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.meta.count").Equal(4)
		obj.Value("data").Array().Length().Equal(4)
		obj.Value("links").Object().NotContainsKey("next")

		// Use the page limit
		obj = e.GET("/data/"+doc.DocType()+"/"+doc.ID()+"/relationships/references").
			WithQuery("page[limit]", 3).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.meta.count").Equal(4)
		obj.Value("data").Array().Length().Equal(3)
		rawNext := obj.Value("links").Object().Value("next").String().NotEmpty().Raw()

		nextURL, err := url.Parse(rawNext)
		require.NoError(t, err)

		// Use the bookmark
		obj = e.GET(nextURL.Path).
			WithQueryString(nextURL.RawQuery).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Value("data").Array().Length().Equal(1)
		obj.Value("links").Object().NotContainsKey("next")

		// Include the files
		obj = e.GET("/data/"+doc.DocType()+"/"+doc.ID()+"/relationships/references").
			WithQuery("include", "files").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Value("included").Array().Length().Equal(4)
		obj.Path("$.included[0].id").String().NotEmpty()
	})

	t.Run("FilePermissions", func(t *testing.T) {
		for _, kind := range []string{"file", "directory"} {
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
				t.Run(kind+"/"+method, func(t *testing.T) {
					e := testutils.CreateTestClient(t, ts.URL)
					doc := getDocForTest(Type, testInstance)
					ref := couchdb.DocReference{Type: Type, ID: doc.ID()}
					var target couchdb.Doc
					if kind == "directory" {
						dir, err := vfs.NewDirDoc(testInstance.VFS(), doc.ID(), consts.RootDirID, nil)
						require.NoError(t, err)
						if method != http.MethodPost {
							dir.AddReferencedBy(ref)
						}
						require.NoError(t, testInstance.VFS().CreateDir(dir))
						target = dir
					} else {
						id := makeReferencedTestFile(t, testInstance, doc, doc.ID()+".jpg")
						file, err := testInstance.VFS().FileByID(id)
						require.NoError(t, err)
						if method == http.MethodPost {
							file.RemoveReferencedBy(ref)
							require.NoError(t, couchdb.UpdateDoc(testInstance, file))
						}
						target = file
					}

					path := "/data/" + Type + "/" + doc.ID() + "/relationships/references"
					docScope := Type + ":ALL:" + doc.ID()
					verb, wrongVerb, success := "PATCH", "GET", http.StatusNoContent
					if method == http.MethodGet {
						verb, wrongVerb, success = "GET", "PATCH", http.StatusOK
					}
					denied := []struct {
						name  string
						scope string
					}{
						{name: "DocumentOnly", scope: docScope},
						{name: "OtherFile", scope: docScope + " io.cozy.files:ALL:other-file"},
						{name: "WrongVerb", scope: docScope + " io.cozy.files:" + wrongVerb},
					}
					if method == http.MethodPost {
						denied = append(denied, struct{ name, scope string }{
							name:  "CannotGrantOwnAccess",
							scope: docScope + " io.cozy.files:ALL:" + doc.ID() + ":referenced_by",
						})
					}
					for _, tc := range denied {
						t.Run(tc.name, func(t *testing.T) {
							e := testutils.CreateTestClient(t, ts.URL)
							_, restricted := setup.GetTestClient(tc.scope)
							e.Request(method, path).
								WithQuery("include", "files").
								WithHeader("Authorization", "Bearer "+restricted).
								WithHeader("Content-Type", "application/vnd.api+json").
								WithJSON(M{"data": S{M{"type": consts.Files, "id": target.ID()}}}).
								Expect().Status(http.StatusForbidden)
							dir, file, err := testInstance.VFS().DirOrFileByID(target.ID())
							require.NoError(t, err)
							if dir != nil {
								assert.Equal(t, target.Rev(), dir.Rev())
							} else {
								assert.Equal(t, target.Rev(), file.Rev())
							}
						})
					}

					for _, scope := range []string{
						docScope + " io.cozy.files:" + verb + ":" + target.ID(),
						"io.cozy.files:" + verb,
					} {
						_, scoped := setup.GetTestClient(scope)
						e.Request(method, path).
							WithQuery("include", "files").
							WithHeader("Authorization", "Bearer "+scoped).
							WithHeader("Content-Type", "application/vnd.api+json").
							WithJSON(M{"data": S{M{"type": consts.Files, "id": target.ID()}}}).
							Expect().Status(success)
					}

					if method == http.MethodGet {
						_, shared := setup.GetTestClient(docScope + " io.cozy.files:GET:" + doc.ID() + ":referenced_by")
						obj := e.GET(path).WithQuery("include", "files").
							WithHeader("Authorization", "Bearer "+shared).
							Expect().Status(http.StatusOK).
							JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
							Object()
						obj.Value("included").Array().Length().Equal(1)
						if kind == "file" {
							obj.Path("$.included[0].links.large").String().Contains("/thumbnails/")
						}
					}
				})
			}
		}
	})

	t.Run("AddReferencesHandler", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Make doc
		doc := getDocForTest(Type, testInstance)

		// Make File
		name := "testtoref.txt"
		dirID := consts.RootDirID
		mime, class := vfs.ExtractMimeAndClassFromFilename(name)
		filedoc, err := vfs.NewFileDoc(name, dirID, -1, nil, mime, class, time.Now(), false, false, false, nil)
		require.NoError(t, err)

		f, err := testInstance.VFS().CreateFile(filedoc, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		// update it
		e.POST("/data/"+doc.DocType()+"/"+doc.ID()+"/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "id": "` + filedoc.ID() + `",
          "type": "` + filedoc.DocType() + `"
        }
      }`)).
			Expect().Status(204)

		fdoc, err := testInstance.VFS().FileByID(filedoc.ID())
		assert.NoError(t, err)
		assert.Len(t, fdoc.ReferencedBy, 1)
	})

	t.Run("RemoveReferencesHandler", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Make doc
		doc := getDocForTest(Type, testInstance)

		// Make Files
		f6 := makeReferencedTestFile(t, testInstance, doc, "testtoref6.txt")
		f7 := makeReferencedTestFile(t, testInstance, doc, "testtoref7.txt")
		f8 := makeReferencedTestFile(t, testInstance, doc, "testtoref8.txt")
		f9 := makeReferencedTestFile(t, testInstance, doc, "testtoref9.txt")

		// update it
		e.DELETE("/data/"+doc.DocType()+"/"+doc.ID()+"/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": [
          {"id": "` + f8 + `", "type": "` + consts.Files + `"},
          {"id": "` + f6 + `", "type": "` + consts.Files + `"}
        ]
      }`)).
			Expect().Status(204)

		fdoc6, err := testInstance.VFS().FileByID(f6)
		assert.NoError(t, err)
		assert.Len(t, fdoc6.ReferencedBy, 0)
		fdoc8, err := testInstance.VFS().FileByID(f8)
		assert.NoError(t, err)
		assert.Len(t, fdoc8.ReferencedBy, 0)

		fdoc7, err := testInstance.VFS().FileByID(f7)
		assert.NoError(t, err)
		assert.Len(t, fdoc7.ReferencedBy, 1)
		fdoc9, err := testInstance.VFS().FileByID(f9)
		assert.NoError(t, err)
		assert.Len(t, fdoc9.ReferencedBy, 1)
	})

	t.Run("ReferencesWithSlash", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Make File
		name := "test-ref-with-slash.txt"
		dirID := consts.RootDirID
		mime, class := vfs.ExtractMimeAndClassFromFilename(name)
		filedoc, err := vfs.NewFileDoc(name, dirID, -1, nil, mime, class, time.Now(), false, false, false, nil)
		require.NoError(t, err)

		f, err := testInstance.VFS().CreateFile(filedoc, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		// Add a reference to io.cozy.apps/foobar
		e.POST("/data/"+Type+"/io.cozy.apps%2ffoobar/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": [
          {"id": "` + filedoc.ID() + `", "type": "` + filedoc.DocType() + `"}
        ]
      }`)).
			Expect().Status(204)

		fdoc, err := testInstance.VFS().FileByID(filedoc.ID())
		assert.NoError(t, err)
		assert.Len(t, fdoc.ReferencedBy, 1)
		assert.Equal(t, "io.cozy.apps/foobar", fdoc.ReferencedBy[0].ID)

		// Check that we can find the reference with /
		obj := e.GET("/data/"+Type+"/io.cozy.apps%2ffoobar/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.meta.count").Equal(1)
		obj.Value("data").Array().Length().Equal(1)
		obj.Path("$.data[0].id").Equal(fdoc.ID())

		// Try again, but this time encode / as %2F instead of %2f
		obj = e.GET("/data/"+Type+"/io.cozy.apps%2Ffoobar/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.meta.count").Equal(1)
		obj.Value("data").Array().Length().Equal(1)
		obj.Path("$.data[0].id").Equal(fdoc.ID())

		// Add dummy references on io.cozy.apps%2ffoobaz and io.cozy.apps%2Ffooqux
		foobazRef := couchdb.DocReference{
			ID:   "io.cozy.apps%2ffoobaz",
			Type: Type,
		}
		fooquxRef := couchdb.DocReference{
			ID:   "io.cozy.apps%2Ffooqux",
			Type: Type,
		}
		fdoc.ReferencedBy = append(fdoc.ReferencedBy, foobazRef, fooquxRef)
		err = couchdb.UpdateDoc(testInstance, fdoc)
		assert.NoError(t, err)

		// Check that we can find the reference with %2f
		obj = e.GET("/data/"+Type+"/io.cozy.apps%2ffoobar/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.meta.count").Equal(1)
		obj.Value("data").Array().Length().Equal(1)
		obj.Path("$.data[0].id").Equal(fdoc.ID())

		// Check that we can find the reference with %2F
		obj = e.GET("/data/"+Type+"/io.cozy.apps%2Ffoobar/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.meta.count").Equal(1)
		obj.Value("data").Array().Length().Equal(1)
		obj.Path("$.data[0].id").Equal(fdoc.ID())

		// Remove the reference with a /
		e.DELETE("/data/"+Type+"/io.cozy.apps%2Ffoobar/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": [
          {"id": "` + fdoc.ID() + `", "type": "` + consts.Files + `"}
        ]
      }`)).
			Expect().Status(204)

		// Remove the reference with a %2f
		e.DELETE("/data/"+Type+"/io.cozy.apps%2ffoobaz/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": [
          {"id": "` + fdoc.ID() + `", "type": "` + consts.Files + `"}
        ]
      }`)).
			Expect().Status(204)

		// Remove the reference with a %2F
		e.DELETE("/data/"+Type+"/io.cozy.apps%2Ffooqux/relationships/references").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": [
          {"id": "` + fdoc.ID() + `", "type": "` + consts.Files + `"}
        ]
      }`)).
			Expect().Status(204)

		// Check that all the references have been removed
		fdoc2, err := testInstance.VFS().FileByID(fdoc.ID())
		assert.NoError(t, err)
		assert.Len(t, fdoc2.ReferencedBy, 0)
	})
}

func makeReferencedTestFile(t *testing.T, instance *instance.Instance, doc couchdb.Doc, name string) string {
	dirID := consts.RootDirID
	mime, class := vfs.ExtractMimeAndClassFromFilename(name)
	filedoc, err := vfs.NewFileDoc(name, dirID, -1, nil, mime, class, time.Now(), false, false, false, nil)
	if !assert.NoError(t, err) {
		return ""
	}

	filedoc.ReferencedBy = []couchdb.DocReference{
		{
			ID:   doc.ID(),
			Type: doc.DocType(),
		},
	}

	f, err := instance.VFS().CreateFile(filedoc, nil)
	if !assert.NoError(t, err) {
		return ""
	}
	if err = f.Close(); !assert.NoError(t, err) {
		return ""
	}
	return filedoc.ID()
}
