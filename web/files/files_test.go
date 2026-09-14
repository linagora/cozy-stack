package files

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/cozy/cozy-stack/web/statik"
	_ "github.com/cozy/cozy-stack/worker/thumbnail"
)

func expectedTrashDisplayName(t *testing.T, inst *instance.Instance) string {
	t.Helper()
	name, err := inst.SettingsPublicName()
	require.NoError(t, err)
	if name == "" {
		name = inst.Domain
	}
	return name
}

func TestFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	var imgID string
	var token string
	var fileID string

	config.UseTestFile(t)
	require.NoError(t, loadLocale(), "Could not load default locale translations")

	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())

	config.GetConfig().Fs.URL = &url.URL{
		Scheme: "file",
		Host:   "localhost",
		Path:   t.TempDir(),
	}

	testInstance := setup.GetTestInstance()
	_, tok := setup.GetTestClient(consts.Files + " " + consts.CertifiedCarbonCopy + " " + consts.CertifiedElectronicSafe)
	token = tok
	ts := setup.GetTestServer("/files", Routes, func(r *echo.Echo) *echo.Echo {
		secure := middlewares.Secure(&middlewares.SecureConfig{
			CSPDefaultSrc:     []middlewares.CSPSource{middlewares.CSPSrcSelf},
			CSPFrameAncestors: []middlewares.CSPSource{middlewares.CSPSrcNone},
		})
		r.Use(secure)
		return r
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("Changes", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create dir "/foo"
		fooID := e.POST("/files/").
			WithQuery("Name", "foo").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create dir "/foo/bar"
		barID := e.POST("/files/"+fooID).
			WithQuery("Name", "bar").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the file "/foo/bar/baz"
		e.POST("/files/"+barID).
			WithQuery("Name", "baz").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(201)

		// Create dir "/foo/qux"
		quxID := e.POST("/files/"+fooID).
			WithQuery("Name", "quz").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Delete dir "/foo/qux"
		e.DELETE("/files/"+quxID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Empty Trash
		e.DELETE("/files/trash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)

		obj := e.GET("/files/_changes").
			WithQuery("include_docs", true).
			WithQuery("include_file_path", true).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().Object()

		obj.Value("last_seq").String().NotEmpty()
		obj.Value("pending").Number()
		results := obj.Value("results").Array()

		// Check if there is a deleted doc
		results.Find(func(_ int, value *httpexpect.Value) bool {
			value.Object().Value("id").String().NotEmpty()
			value.Object().ValueEqual("deleted", true)
			return true
		}).
			NotNull()

		// Check if we can find the trashed "/foo/qux"
		results.Find(func(_ int, value *httpexpect.Value) bool {
			doc := value.Object().Value("doc").Object()
			doc.ValueEqual("type", "directory")
			doc.ValueEqual("path", "/.cozy_trash")
			return true
		}).
			NotNull()

		obj = e.GET("/files/_changes").
			WithQuery("include_docs", true).
			WithQuery("fields", "type,name,dir_id").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().
			Object()

		obj.Value("last_seq").String().NotEmpty()
		obj.Value("pending").Number()

		results = obj.Value("results").Array()
		results.Every(func(_ int, value *httpexpect.Value) {
			res := value.Object()

			res.Value("id").String().NotEmpty()

			// Skip the deleted entry and the root dir
			if _, ok := res.Raw()["deleted"]; ok || res.Value("id").String().Raw() == "io.cozy.files.root-dir" {
				return
			}

			doc := res.Value("doc").Object()
			doc.Value("type").String().NotEmpty()
			doc.Value("name").String().NotEmpty()
			doc.Value("dir_id").String().NotEmpty()
			doc.NotContainsKey("path")
			doc.NotContainsKey("metadata")
			doc.NotContainsKey("created_at")
		})

		// Delete dir "/foo/bar"
		e.DELETE("/files/"+barID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		obj = e.GET("/files/_changes").
			WithQuery("include_docs", true).
			WithQuery("skip_deleted", "true").
			WithQuery("skip_trashed", "true").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON().
			Object()

		obj.Value("last_seq").String().NotEmpty()
		obj.Value("pending").Number()

		results = obj.Value("results").Array()

		// Check if there is a deleted doc
		results.NotFind(func(_ int, value *httpexpect.Value) bool {
			value.Object().ValueEqual("deleted", true)
			return true
		})

		// Check if we can find a trashed file
		results.NotFind(func(_ int, value *httpexpect.Value) bool {
			doc := value.Object().Value("doc").Object()
			doc.Value("path").String().HasPrefix("/.cozy_trash")
			return true
		})

		// Delete dir "/foo"
		e.DELETE("/files/"+fooID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Empty Trash
		e.DELETE("/files/trash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)
	})

	t.Run("CreateDirWithNoType", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(422)
	})

	t.Run("CreateDirWithNoName", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(422)
	})

	t.Run("CreateDirOnNonExistingParent", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/noooooop").
			WithQuery("Name", "foo").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)
	})

	t.Run("CreateDirAlreadyExists", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "iexist").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		e.POST("/files/").
			WithQuery("Name", "iexist").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(409)
	})

	t.Run("CreateDirRootSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "coucou").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		storage := testInstance.VFS()
		exists, err := vfs.DirExists(storage, "/coucou")
		assert.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("CreateDirWithDateSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Name", "dir-with-date").
			WithQuery("Type", "directory").
			WithQuery("CreatedAt", "2016-09-18T10:24:53Z").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Date", "Mon, 19 Sep 2016 12:35:08 GMT").
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("created_at", "2016-09-18T10:24:53Z")
		attrs.ValueEqual("updated_at", "2016-09-19T12:35:08Z")

		fcm := attrs.Value("cozyMetadata").Object()
		fcm.ValueEqual("metadataVersion", 1.0)
		fcm.ValueEqual("doctypeVersion", "1")
		fcm.Value("createdOn").String().Contains(testInstance.Domain)
		fcm.Value("createdAt").String().DateTime(time.RFC3339)
		fcm.Value("updatedAt").String().DateTime(time.RFC3339)
		fcm.NotContainsKey("uploadedAt")
	})

	t.Run("CreateDirWithDateSuccessAndUpdatedAt", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Type", "directory").
			WithQuery("Name", "dir-with-date-and-updatedat").
			WithQuery("CreatedAt", "2016-09-18T10:24:53Z").
			WithQuery("UpdatedAt", "2020-05-12T12:25:00Z").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Date", "Mon, 19 Sep 2016 12:35:08 GMT").
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("created_at", "2016-09-18T10:24:53Z")
		attrs.ValueEqual("updated_at", "2020-05-12T12:25:00Z")

		fcm := attrs.Value("cozyMetadata").Object()
		fcm.ValueEqual("metadataVersion", 1.0)
		fcm.ValueEqual("doctypeVersion", "1")
		fcm.Value("createdOn").String().Contains(testInstance.Domain)
		fcm.Value("createdAt").String().DateTime(time.RFC3339)
		fcm.Value("updatedAt").String().DateTime(time.RFC3339)
		fcm.NotContainsKey("uploadedAt")
	})

	t.Run("CreateDirWithParentSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create dir "/dirparent"
		parentID := e.POST("/files/").
			WithQuery("Name", "dirparent").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create dir "/dirparent/child"
		e.POST("/files/"+parentID).
			WithQuery("Name", "child").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		storage := testInstance.VFS()
		exists, err := vfs.DirExists(storage, "/dirparent/child")
		assert.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("CreateDirWithIllegalCharacter", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "coucou/with/slashs!").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(422)
	})

	t.Run("CreateDirWithMetadata", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/upload/metadata").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
            "type": "io.cozy.files.metadata",
            "attributes": {
                "device-id": "123456789"
            }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.ValueEqual("type", consts.FilesMetadata)
		attrs := data.Value("attributes").Object()
		attrs.ValueEqual("device-id", "123456789")
		secret := data.Value("id").String().NotEmpty().Raw()

		obj = e.POST("/files/").
			WithQuery("Name", "dir-with-metadata").
			WithQuery("Type", "directory").
			WithQuery("MetadataID", secret).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		dirID := obj.Path("$.data.id").String().NotEmpty().Raw()
		meta := obj.Path("$.data.attributes.metadata").Object()
		meta.ValueEqual("device-id", "123456789")

		// Check that the metadata are still here after an update
		obj = e.PATCH("/files/"+dirID).
			WithQuery("Path", "/dir-with-metadata").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "file",
          "id": "` + dirID + `",
          "attributes": {
            "name": "new-name-for-dir-with-metadata"
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		meta = obj.Path("$.data.attributes.metadata").Object()
		meta.ValueEqual("device-id", "123456789")

		obj = e.GET("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		meta = obj.Path("$.data.attributes.metadata").Object()
		meta.ValueEqual("device-id", "123456789")
	})

	t.Run("PatchDirMetadata", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create a directory
		obj := e.POST("/files/").
			WithQuery("Name", "meta-patch-dir").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		dirID := obj.Path("$.data.id").String().NotEmpty().Raw()

		// Initially, metadata key is absent
		obj.Path("$.data.attributes").Object().NotContainsKey("metadata")

		// Patch metadata on the directory
		obj = e.PATCH("/files/"+dirID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "directory",
          "id": "` + dirID + `",
          "attributes": {
            "metadata": { "decoration": { "color": "#FFF" } }
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		meta := obj.Path("$.data.attributes.metadata").Object()
		meta.Value("decoration").Object().HasValue("color", "#FFF")

		// Replacing metadata should drop previous keys (no deep merge)
		obj = e.PATCH("/files/"+dirID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "directory",
          "id": "` + dirID + `",
          "attributes": {
            "metadata": { "only": true }
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		meta = obj.Path("$.data.attributes.metadata").Object()
		meta.HasValue("only", true)
		meta.NotContainsKey("decoration")
	})

	t.Run("CreateDirConcurrently", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		done := make(chan int)
		errs := make(chan int)

		doCreateDir := func(name string) {
			res := e.POST("/files/").
				WithQuery("Name", name).
				WithQuery("Type", "directory").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Raw()
			_ = res.Body.Close()

			if res.StatusCode == 201 {
				done <- res.StatusCode
			} else {
				errs <- res.StatusCode
			}
		}

		n := 100
		c := 0

		for i := 0; i < n; i++ {
			go doCreateDir("foo")
		}

		for i := 0; i < n; i++ {
			select {
			case res := <-errs:
				assert.True(t, res == 409 || res == 503)
			case <-done:
				c += 1
			}
		}

		assert.Equal(t, 1, c)
	})

	t.Run("UploadWithNoType", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "baz").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(422)
	})

	t.Run("UploadWithNoName", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(422)
	})

	t.Run("UploadToNonExistingParent", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/noooop").
			WithQuery("Type", "file").
			WithQuery("Name", "no-parent").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(404)
	})

	t.Run("UploadWithInvalidContentType", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "invalid-mime").
			WithHeader("Content-Type", "foo € / bar").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(422)
	})

	t.Run("UploadToTrashedFolder", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create dir "/foo"
		dirID := e.POST("/files/").
			WithQuery("Name", "trashed-parent").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		e.POST("/files/"+dirID).
			WithQuery("Type", "file").
			WithQuery("Name", "foo").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(404)
	})

	t.Run("UploadBadSize", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "bad-size").
			WithQuery("Size", 42).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithTransformer(func(r *http.Request) { r.ContentLength = -1 }).
			WithBytes([]byte("baz")). // not 42 byte
			Expect().Status(412)

		storage := testInstance.VFS()
		_, err := readFile(storage, "/badsize")
		assert.Error(t, err)
	})

	t.Run("UploadBadHash", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "bad-hash").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "3FbbMXfH+PdjAlWFfVb1dQ=="). // invalid md5
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(412)

		storage := testInstance.VFS()
		_, err := readFile(storage, "/badhash")
		assert.Error(t, err)
	})

	t.Run("UploadAtRootSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "goodhash").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201)

		storage := testInstance.VFS()
		buf, err := readFile(storage, "/goodhash")
		assert.NoError(t, err)
		assert.Equal(t, "foo", string(buf))
	})

	t.Run("UploadExceedingQuota", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		lifecycle.Patch(testInstance, &lifecycle.Options{DiskQuota: 3})

		e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "too-large").
			WithQuery("Size", 3).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithTransformer(func(r *http.Request) { r.ContentLength = -1 }).
			WithBytes([]byte("baz")).
			Expect().
			Status(413).
			Body().Contains(vfs.ErrFileTooBig.Error())

		storage := testInstance.VFS()
		_, err := readFile(storage, "/too-large")
		assert.Error(t, err)

		lifecycle.Patch(testInstance, &lifecycle.Options{DiskQuota: -1})
	})

	t.Run("UploadImage", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		rawFile, err := os.ReadFile("../../tests/fixtures/wet-cozy_20160910__M4Dz.jpg")
		require.NoError(t, err)

		obj := e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "wet.jpg").
			WithQuery("Metadata", `{"gps":{"city":"Paris","country":"France"}}`).
			WithHeader("Content-MD5", "tHWYYuXBBflJ8wXgJ2c2yg==").
			WithHeader("Content-Type", "image/jpeg").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes(rawFile).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		imgID = data.Value("id").String().NotEmpty().Raw()

		data.Path("$.attributes.created_at").String().HasPrefix("2016-09-10T")

		meta := data.Path("$.attributes.metadata").Object()
		meta.ValueEqual("extractor_version", float64(vfs.MetadataExtractorVersion))
		meta.ValueEqual("flash", "Off, Did not fire")

		gps := meta.Value("gps").Object()
		gps.ValueEqual("city", "Paris")
		gps.ValueEqual("country", "France")
	})

	t.Run("UploadShortcut", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		rawFile, err := os.ReadFile("../../tests/fixtures/shortcut.url")
		require.NoError(t, err)

		obj := e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "shortcut.url").
			WithHeader("Content-MD5", "+tHtr9V8+4gcCDxTFAqt3w==").
			WithHeader("Content-Type", "application/octet-stream").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes(rawFile).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("mime", "application/internet-shortcut")
		attrs.ValueEqual("class", "shortcut")
	})

	t.Run("UploadWithParentSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		parentID := e.POST("/files/").
			WithQuery("Name", "fileparent").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		e.POST("/files/"+parentID).
			WithQuery("Name", "goodhash").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201)

		storage := testInstance.VFS()
		buf, err := readFile(storage, "/fileparent/goodhash")
		assert.NoError(t, err)
		assert.Equal(t, "foo", string(buf))
	})

	t.Run("UploadAtRootAlreadyExists", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "iexistfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201)

		// Same file
		e.POST("/files/").
			WithQuery("Name", "iexistfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(409)
	})

	t.Run("UploadWithParentAlreadyExists", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		parentID := e.POST("/files/").
			WithQuery("Name", "container").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		e.POST("/files/"+parentID).
			WithQuery("Name", "iexistfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201)

		// Same file, same path
		e.POST("/files/"+parentID).
			WithQuery("Name", "iexistfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(409)
	})

	t.Run("UploadWithCreatedAtAndHeaderDate", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Name", "withcdate").
			WithQuery("Type", "file").
			WithQuery("CreatedAt", "2016-09-18T10:24:53Z").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Date", "Mon, 19 Sep 2016 12:38:04 GMT").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("created_at", "2016-09-18T10:24:53Z")
		attrs.ValueEqual("updated_at", "2016-09-19T12:38:04Z")
	})

	t.Run("UploadWithCreatedAtAndUpdatedAt", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Name", "TestUploadWithCreatedAtAndUpdatedAt").
			WithQuery("Type", "file").
			WithQuery("CreatedAt", "2016-09-18T10:24:53Z").
			WithQuery("UpdatedAt", "2020-05-12T12:25:00Z").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("created_at", "2016-09-18T10:24:53Z")
		attrs.ValueEqual("updated_at", "2020-05-12T12:25:00Z")
	})

	t.Run("UploadWithCreatedAtAndUpdatedAtAndDateHeader", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Name", "TestUploadWithCreatedAtAndUpdatedAtAndDateHeader").
			WithQuery("Type", "file").
			WithQuery("CreatedAt", "2016-09-18T10:24:53Z").
			WithQuery("UpdatedAt", "2020-05-12T12:25:00Z").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Date", "Mon, 19 Sep 2016 12:38:04 GMT").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("created_at", "2016-09-18T10:24:53Z")
		attrs.ValueEqual("updated_at", "2020-05-12T12:25:00Z")
	})

	t.Run("UploadWithMetadata", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/upload/metadata").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
            "type": "io.cozy.files.metadata",
            "attributes": {
                "category": "report",
                "subCategory": "theft",
                "datetime": "2017-04-22T01:00:00-05:00",
                "label": "foobar"
            }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.ValueEqual("type", consts.FilesMetadata)

		secret := data.Value("id").String().NotEmpty().Raw()

		attrs := data.Value("attributes").Object()
		attrs.ValueEqual("category", "report")
		attrs.ValueEqual("subCategory", "theft")
		attrs.ValueEqual("label", "foobar")
		attrs.ValueEqual("datetime", "2017-04-22T01:00:00-05:00")

		obj = e.POST("/files/").
			WithQuery("Name", "withmetadataid").
			WithQuery("Type", "file").
			WithQuery("MetadataID", secret).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		meta := obj.Path("$.data.attributes.metadata").Object()
		meta.ValueEqual("category", "report")
		meta.ValueEqual("subCategory", "theft")
		meta.ValueEqual("label", "foobar")
		meta.ValueEqual("datetime", "2017-04-22T01:00:00-05:00")
	})

	t.Run("UploadWithSourceAccount", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		account := "0c5a0a1e-8eb1-11e9-93f3-934f3a2c181d"
		identifier := "11f68e48"

		obj := e.POST("/files/").
			WithQuery("Name", "with-sourceAccount").
			WithQuery("Type", "file").
			WithQuery("SourceAccount", account).
			WithQuery("SourceAccountIdentifier", identifier).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fcm := obj.Path("$.data.attributes.cozyMetadata").Object()
		fcm.ValueEqual("sourceAccount", account)
		fcm.ValueEqual("sourceAccountIdentifier", identifier)
	})

	t.Run("CopyFile", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "copyFileDir").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		fileName := "bar"
		fileExt := ".txt"
		fileContent := "file content"

		// 1. Upload file and get its id
		obj := e.POST("/files/").
			WithQuery("Name", fileName+fileExt).
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fileContent)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()
		file1Attrs := obj.Path("$.data.attributes").Object()

		// 2. Send file copy request
		obj = e.POST("/files/"+fileID+"/copy").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		copyID := obj.Path("$.data.id").String().NotEmpty().NotEqual(fileID).Raw()

		// 3. Fetch copy metadata and compare with file
		obj = e.GET("/files/"+copyID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.relationships").Object().NotContainsKey("old_versions")

		attrs := obj.Path("$.data.attributes").Object()
		attrs.Value("created_at").String().NotEmpty().
			NotEqual(file1Attrs.Value("created_at").String().Raw()).
			DateTime(time.RFC3339)
		attrs.ValueEqual("dir_id", file1Attrs.Value("dir_id").String().Raw())
		attrs.ValueEqual("name", fileName+" (copy)"+fileExt)

		// 4. fetch copy and check its content
		res := e.GET("/files/download/"+copyID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		res.Header("Content-Disposition").HasPrefix("inline")
		res.Header("Content-Disposition").Contains(`filename="` + fileName + "(copy)" + fileExt + `"`)
		res.Header("Content-Type").Contains("text/plain")
		res.Header("Etag").NotEmpty()
		res.Header("Content-Length").Equal(strconv.Itoa(len(fileContent)))
		res.Body().Equal(fileContent)

		// 5. Send file copy request specifying copy name and parent id
		destDirID := e.POST("/files/").
			WithQuery("Name", "destDir").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		copyName := "My-file-copy"

		obj = e.POST("/files/"+fileID+"/copy").
			WithQuery("Name", copyName).
			WithQuery("DirID", destDirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		copyID = obj.Path("$.data.id").String().NotEqual(fileID).Raw()

		// 6. Fetch copy metadata and compare with file
		obj = e.GET("/files/"+copyID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs = obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("dir_id", destDirID)
		attrs.ValueEqual("name", copyName)
	})

	t.Run("ModifyMetadataByPath", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		fileID = e.POST("/files/").
			WithQuery("Name", "file-move-me-by-path").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		dirID := e.POST("/files/").
			WithQuery("Name", "move-by-path").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		obj := e.PATCH("/files/metadata").
			WithQuery("Path", "/file-move-me-by-path").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "file",
          "id": "` + fileID + `",
          "attributes": {
            "tags": ["bar", "bar", "baz"],
            "name": "moved",
            "dir_id": "` + dirID + `",
            "executable": true,
            "cozyMetadata": {
              "favorite": true
            }
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("mime", "text/plain")
		attrs.ValueEqual("name", "moved")
		attrs.ValueEqual("tags", []string{"bar", "baz"})
		attrs.ValueEqual("class", "text")
		attrs.ValueEqual("md5sum", "rL0Y20zC+Fzt72VPzMSk2A==")
		attrs.ValueEqual("executable", true)
		attrs.ValueEqual("size", "3")
		attrs.Value("cozyMetadata").Object().ValueEqual("favorite", true)
	})

	t.Run("ModifyMetadataFileMove", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		fileID = e.POST("/files/").
			WithQuery("Name", "filemoveme").
			WithQuery("Type", "file").
			WithQuery("Tags", "foo,bar").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		dirID := e.POST("/files/").
			WithQuery("Name", "movemeinme").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		obj := e.PATCH("/files/"+fileID).
			WithQuery("Path", "/file-move-me-by-path").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "file",
          "id": "` + fileID + `",
          "attributes": {
            "tags": ["bar", "bar", "baz"],
            "name": "moved",
            "dir_id": "` + dirID + `",
            "executable": true
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("mime", "text/plain")
		attrs.ValueEqual("name", "moved")
		attrs.ValueEqual("tags", []string{"bar", "baz"})
		attrs.ValueEqual("class", "text")
		attrs.ValueEqual("md5sum", "rL0Y20zC+Fzt72VPzMSk2A==")
		attrs.ValueEqual("executable", true)
		attrs.ValueEqual("size", "3")
	})

	t.Run("ModifyMetadataFileConflict", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		fileID = e.POST("/files/").
			WithQuery("Name", "fmodme1").
			WithQuery("Type", "file").
			WithQuery("Tags", "foo,bar").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		e.POST("/files/").
			WithQuery("Name", "fmodme2").
			WithQuery("Type", "file").
			WithQuery("Tags", "foo,bar").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		// Try to rename fmodme1 into the existing fmodme2
		e.PATCH("/files/"+fileID).
			WithQuery("Path", "/file-move-me-by-path").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "file",
          "id": "` + fileID + `",
          "attributes": {
            "name": "fmodme2"
          }
        }
      }`)).
			Expect().Status(409)
	})

	t.Run("ModifyMetadataDirMove", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create dir "/dirmodme"
		dir1ID := e.POST("/files/").
			WithQuery("Name", "dirmodme").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create dir "/dirmodme/child1"
		e.POST("/files/"+dir1ID).
			WithQuery("Name", "child1").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create dir "/dirmodme/child2"
		e.POST("/files/"+dir1ID).
			WithQuery("Name", "child2").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create dir "/dirmodmemoveinme"
		dir2ID := e.POST("/files/").
			WithQuery("Name", "dirmodmemoveinme").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Move the folder "/dirmodme" into "/dirmodmemoveinme"
		e.PATCH("/files/"+dir1ID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "directory",
          "id": "` + dir1ID + `",
          "attributes": {
            "tags": ["bar", "baz"],
            "name": "renamed",
            "dir_id": "` + dir2ID + `"
          }
        }
      }`)).
			Expect().Status(200)

		storage := testInstance.VFS()
		exists, err := vfs.DirExists(storage, "/dirmodmemoveinme/renamed")
		assert.NoError(t, err)
		assert.True(t, exists)

		// Try to move the folder "/dirmodmemoveinme" into it sub folder "/dirmodmemoveinme/dirmodme"
		e.PATCH("/files/"+dir2ID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "directory",
          "id": "` + dir2ID + `",
          "attributes": {
            "tags": ["bar", "baz"],
            "name": "rename",
            "dir_id": "` + dir1ID + `"
          }
        }
      }`)).
			Expect().Status(412)

		// Try to move the folder "/dirmodme" into itself
		e.PATCH("/files/"+dir1ID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "directory",
          "id": "` + dir1ID + `",
          "attributes": {
            "tags": ["bar", "baz"],
            "name": "rename",
            "dir_id": "` + dir1ID + `"
          }
        }
      }`)).
			Expect().Status(412)
	})

	t.Run("ModifyMetadataDirMoveWithRel", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create dir "/dirmodmewithrel"
		dir1ID := e.POST("/files/").
			WithQuery("Name", "dirmodmewithrel").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create dir "/dirmodmewithrel/child1"
		e.POST("/files/"+dir1ID).
			WithQuery("Name", "child1").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create dir "/dirmodmewithrel/child2"
		e.POST("/files/"+dir1ID).
			WithQuery("Name", "child2").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create dir "/dirmodmemoveinmewithrel"
		dir2ID := e.POST("/files/").
			WithQuery("Name", "dirmodmemoveinmewithrel").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Move the folder "/dirmodme" into "/dirmodmemoveinme"
		e.PATCH("/files/"+dir1ID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "directory",
          "id": "` + dir1ID + `",
          "attributes": {
            "type": "io.cozy.files",
            "dir_id": "` + dir2ID + `"
          }
        }
      }`)).
			Expect().Status(200)

		storage := testInstance.VFS()
		exists, err := vfs.DirExists(storage, "/dirmodmemoveinmewithrel/dirmodmewithrel")
		assert.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("ModifyMetadataDirMoveConflict", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create dir "/conflictmodme1"
		e.POST("/files/").
			WithQuery("Name", "conflictmodme1").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		// Create dir "/conflictmodme2"
		dir2ID := e.POST("/files/").
			WithQuery("Name", "conflictmodme2").
			WithQuery("Type", "directory").
			WithQuery("Tags", "foo,bar,bar").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Try to rename "/confictmodme2" into the already taken name "/confilctmodme1"
		e.PATCH("/files/"+dir2ID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "directory",
          "id": "` + dir2ID + `",
          "attributes": {
            "tags": ["bar", "baz"],
            "name": "conflictmodme1"
          }
        }
      }`)).
			Expect().Status(409)
	})

	t.Run("ModifyContentBadRev", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "modbadrev").
			WithQuery("Executable", true).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()
		fileRev := obj.Path("$.data.meta.rev").String().NotEmpty().Raw()

		e.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("If-Match", "badrev"). // invalid
			WithBytes([]byte("newcontent :)")).
			Expect().Status(412)

		e.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("If-Match", fileRev).
			WithBytes([]byte("newcontent :)")).
			Expect().Status(200)
	})

	t.Run("ModifyContentSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		storage := testInstance.VFS()

		obj := e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "willbemodified").
			WithQuery("Executable", true).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data1 := obj.Value("data").Object()
		file1ID := data1.Value("id").String().NotEmpty().Raw()
		attrs1 := data1.Value("attributes").Object()
		uploadedAt1 := attrs1.Path("$.cozyMetadata.uploadedAt").String().NotEmpty().Raw()

		buf, err := readFile(storage, "/willbemodified")
		assert.NoError(t, err)
		assert.Equal(t, "foo", string(buf))

		fileInfo, err := storage.FileByPath("/willbemodified")
		assert.NoError(t, err)
		assert.Equal(t, fileInfo.Mode().String(), "-rwxr-xr-x")

		newContent := "newcontent :)"

		// Upload a new content to the file creating a new version
		obj = e.PUT("/files/"+file1ID).
			WithQuery("Executable", false).
			WithHeader("Content-Type", "audio/mp3").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(newContent)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.ValueEqual("id", data1.Value("id").Raw())
		data.Path("$.links.self").Equal(data1.Path("$.links.self").Raw())

		meta := data.Value("meta").Object()
		meta.Value("rev").NotEqual(data1.Path("$.meta.rev").String().Raw())

		attrs := data.Value("attributes").Object()
		attrs.ValueEqual("name", attrs1.Value("name").String().Raw())
		attrs.ValueEqual("created_at", attrs1.Value("created_at").String().Raw())
		attrs.Value("updated_at").NotEqual(attrs1.Value("updated_at").String().Raw())
		attrs.Value("size").NotEqual(attrs1.Value("size").String().Raw())
		attrs.ValueEqual("size", strconv.Itoa(len(newContent)))
		attrs.Value("md5sum").NotEqual(attrs1.Value("md5sum").String().Raw())
		attrs.Value("class").NotEqual(attrs1.Value("class").String().Raw())
		attrs.Value("mime").NotEqual(attrs1.Value("mime").String().Raw())
		attrs.Value("executable").NotEqual(attrs1.Value("executable").Boolean().Raw())
		attrs.ValueEqual("class", "audio")
		attrs.ValueEqual("mime", "audio/mp3")
		attrs.ValueEqual("executable", false)
		attrs.Path("$.cozyMetadata.uploadedAt").NotEqual(uploadedAt1)

		buf, err = readFile(storage, "/willbemodified")
		assert.NoError(t, err)
		assert.Equal(t, newContent, string(buf))
		fileInfo, err = storage.FileByPath("/willbemodified")
		assert.NoError(t, err)
		assert.Equal(t, fileInfo.Mode().String(), "-rw-r--r--")

		e.PUT("/files/"+fileID).
			WithHeader("Date", "Mon, 02 Jan 2006 15:04:05 MST").
			WithHeader("Content-Type", "what/ever").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("")).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.updated_at").Equal("2006-01-02T15:04:05Z")

		newContent = "encryptedcontent"

		e.PUT("/files/"+fileID).
			WithQuery("Encrypted", true).
			WithHeader("Date", "Mon, 02 Jan 2006 15:04:05 MST").
			WithHeader("Content-Type", "audio/mp3").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(newContent)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.attributes.encrypted").Equal(true)
	})

	t.Run("ModifyContentWithSourceAccount", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		fileID = e.POST("/files/").
			WithQuery("Name", "old-file-to-migrate").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		account := "0c5a0a1e-8eb1-11e9-93f3-934f3a2c181d"
		identifier := "11f68e48"
		newContent := "updated by a konnector to add the sourceAccount"

		obj := e.PUT("/files/"+fileID).
			WithQuery("Name", "old-file-to-migrate").
			WithQuery("SourceAccount", account).
			WithQuery("SourceAccountIdentifier", identifier).
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(newContent)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fcm := obj.Path("$.data.attributes.cozyMetadata").Object()
		fcm.ValueEqual("sourceAccount", account)
		fcm.ValueEqual("sourceAccountIdentifier", identifier)
	})

	t.Run("ModifyContentWithCreatedAt", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Name", "old-file-with-c").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()
		createdAt := obj.Path("$.data.attributes.created_at").String().NotEmpty().Raw()
		updatedAt := obj.Path("$.data.attributes.updated_at").String().NotEmpty().Raw()

		createdAt2 := "2017-11-16T13:37:01.345Z"
		newContent := "updated by a client with a new CreatedAt"

		obj = e.PUT("/files/"+fileID).
			WithQuery("Name", "old-file-to-migrate").
			WithQuery("CreatedAt", createdAt2).
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(newContent)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.created_at").Equal(createdAt)
		obj.Path("$.data.attributes.updated_at").NotEqual(updatedAt)
	})

	t.Run("ModifyContentWithUpdatedAt", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		createdAt := "2017-11-16T13:37:01.345Z"

		fileID = e.POST("/files/").
			WithQuery("Name", "old-file-with-u").
			WithQuery("Type", "file").
			WithQuery("CreatedAt", createdAt).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		updatedAt2 := "2017-12-16T13:37:01.345Z"
		newContent := "updated by a client with a new UpdatedAt"

		obj := e.PUT("/files/"+fileID).
			WithQuery("UpdatedAt", updatedAt2).
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(newContent)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.created_at").Equal(createdAt)
		obj.Path("$.data.attributes.updated_at").Equal(updatedAt2)
	})

	t.Run("ModifyContentWithUpdatedAtAndCreatedAt", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		createdAt := "2017-11-16T13:37:01.345Z"

		fileID = e.POST("/files/").
			WithQuery("Name", "old-file-with-u-and-c").
			WithQuery("Type", "file").
			WithQuery("CreatedAt", createdAt).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		createdAt2 := "2017-10-16T13:37:01.345Z"
		updatedAt2 := "2017-12-16T13:37:01.345Z"
		newContent := "updated by a client with a CreatedAt older than UpdatedAt"

		obj := e.PUT("/files/"+fileID).
			WithQuery("UpdatedAt", updatedAt2).
			WithQuery("CreateddAt", createdAt2).
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(newContent)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.created_at").Equal(createdAt)
		obj.Path("$.data.attributes.updated_at").Equal(updatedAt2)
	})

	t.Run("ModifyContentConcurrently", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		fileID = e.POST("/files/").
			WithQuery("Name", "willbemodifiedconcurrently").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		var c int64

		type resC struct {
			obj *httpexpect.Object
			idx int64
		}

		errs := make(chan int)
		done := make(chan resC)

		doModContent := func() {
			idx := atomic.AddInt64(&c, 1)

			res := e.PUT("/files/"+fileID).
				WithQuery("Type", "file").
				WithHeader("Content-Type", "text/plain").
				WithHeader("Authorization", "Bearer "+token).
				WithBytes([]byte("newcontent " + strconv.FormatInt(idx, 10))).
				Expect()

			rawRes := res.Raw()
			_ = rawRes.Body.Close()

			if rawRes.StatusCode == 200 {
				done <- resC{
					obj: res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object(),
					idx: idx,
				}
			} else {
				errs <- rawRes.StatusCode
			}
		}

		n := 100

		for i := 0; i < n; i++ {
			go doModContent()
		}

		var successes []resC
		for i := 0; i < n; i++ {
			select {
			case res := <-errs:
				assert.True(t, res == 409 || res == 503, "status code is %d and not 409 or 503", res)
			case res := <-done:
				successes = append(successes, res)
			}
		}

		assert.True(t, len(successes) >= 1, "there is at least one success")

		for i, res := range successes {
			res.obj.Path("$.data.meta.rev").String().HasPrefix(strconv.Itoa(i+2) + "-")
		}

		storage := testInstance.VFS()
		buf, err := readFile(storage, "/willbemodifiedconcurrently")
		assert.NoError(t, err)

		found := false
		for _, res := range successes {
			t.Logf("succ: %#v\n\n", res)
			if string(buf) == "newcontent "+strconv.FormatInt(res.idx, 10) {
				found = true
				break
			}
		}

		assert.True(t, found)
	})

	t.Run("DownloadFileBadID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/files/download/badid").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)
	})

	t.Run("DownloadFileBadPath", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/files/download/").
			WithQuery("Path", "/i/do/not/exists").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)
	})

	t.Run("DownloadFileByIDSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		fileID = e.POST("/files/").
			WithQuery("Name", "downloadme1").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		res := e.GET("/files/download/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			ContentType("text/plain", "")

		res.Header("Content-Disposition").HasPrefix("inline")
		res.Header("Content-Disposition").Contains(`filename="downloadme1"`)
		res.Header("Etag").NotEmpty()
		res.Header("Content-Length").Equal("3")
		res.Header("Accept-Ranges").Equal("bytes")

		res.Body().Equal("foo")
	})

	t.Run("DownloadFileByPathSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "downloadme2").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		res := e.GET("/files/download").
			WithQuery("Dl", "1").
			WithQuery("Path", "/downloadme2").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			ContentType("text/plain", "")

		res.Header("Content-Disposition").HasPrefix("attachment")
		res.Header("Content-Disposition").Contains(`filename="downloadme2"`)
		res.Header("Content-Length").Equal("3")
		res.Header("Accept-Ranges").Equal("bytes")

		res.Body().Equal("foo")
	})

	t.Run("DownloadRangeSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "downloadmebyrange").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201)

		e.GET("/files/download").
			WithQuery("Path", "/downloadmebyrange").
			WithQuery("", "/downloadmebyrange").
			WithHeader("Range", "nimp").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(416)

		e.GET("/files/download").
			WithQuery("Path", "/downloadmebyrange").
			WithQuery("", "/downloadmebyrange").
			WithHeader("Range", "bytes=0-2").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(206).
			Body().Equal("foo")

		e.GET("/files/download").
			WithQuery("Path", "/downloadmebyrange").
			WithQuery("", "/downloadmebyrange").
			WithHeader("Range", "bytes=4-").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(206).
			Body().Equal("bar")
	})

	t.Run("GetFileMetadataFromPath", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/files/metadata").
			WithQuery("Path", "/nooooop").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)

		e.POST("/files/").
			WithQuery("Name", "getmetadata").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201)

		e.GET("/files/metadata").
			WithQuery("Path", "/getmetadata").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
	})

	t.Run("GetDirMetadataFromPath", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/").
			WithQuery("Name", "getdirmeta").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		e.GET("/files/metadata").
			WithQuery("Path", "/getdirmeta").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
	})

	t.Run("GetFileMetadataFromID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/files/qsdqsd").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)

		fileID = e.POST("/files/").
			WithQuery("Name", "getmetadatafromid").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
	})

	t.Run("GetDirMetadataFromID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		dirID := e.POST("/files/").
			WithQuery("Name", "getdirmetafromid").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		fileID = e.POST("/files/"+dirID).
			WithQuery("Name", "firstfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
	})

	t.Run("Versions", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		cfg := config.GetConfig()
		oldDelay := cfg.Fs.Versioning.MinDelayBetweenTwoVersions
		cfg.Fs.Versioning.MinDelayBetweenTwoVersions = 10 * time.Millisecond
		t.Cleanup(func() { cfg.Fs.Versioning.MinDelayBetweenTwoVersions = oldDelay })

		obj := e.POST("/files/").
			WithQuery("Name", "versioned").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("one")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()
		sum1 := obj.Path("$.data.attributes.md5sum").String().NotEmpty().Raw()

		time.Sleep(20 * time.Millisecond)

		obj = e.PUT("/files/"+fileID).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("two")).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		sum2 := obj.Path("$.data.attributes.md5sum").String().NotEmpty().Raw()

		time.Sleep(20 * time.Millisecond)

		obj3 := e.PUT("/files/"+fileID).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("three")).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		resObj := e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		resObj.Path("$.data.attributes.md5sum").Equal(obj3.Path("$.data.attributes.md5sum").String().Raw())

		oldRefs := resObj.Path("$.data.relationships.old_versions.data").Array()
		oldRefs.Length().Equal(2)

		first := oldRefs.Element(0).Object()
		first.ValueEqual("type", consts.FilesVersions)
		oneID := first.Value("id").String().NotEmpty().Raw()

		second := oldRefs.Element(1).Object()
		second.ValueEqual("type", consts.FilesVersions)
		secondID := second.Value("id").String().NotEmpty().Raw()

		included := resObj.Value("included").Array()
		included.Length().Equal(2)

		vOne := included.Element(0).Object()
		vOne.ValueEqual("id", oneID)
		vOne.Path("$.attributes.md5sum").Equal(sum1)

		vTwo := included.Element(1).Object()
		vTwo.ValueEqual("id", secondID)
		vTwo.Path("$.attributes.md5sum").Equal(sum2)
	})

	t.Run("PatchVersion", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file
		fileID = e.POST("/files/").
			WithQuery("Name", "patch-version").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("one")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Upload a new version
		e.PUT("/files/"+fileID).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("two")).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Get file informations
		obj := e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		refs := obj.Path("$.data.relationships.old_versions.data").Array()
		refs.Length().Equal(1)

		ref := refs.Element(0).Object()
		ref.ValueEqual("type", consts.FilesVersions)
		versionID := ref.Value("id").String().NotEmpty().Raw()

		obj = e.PATCH("/files/"+versionID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "type": "` + consts.FilesVersions + `",
          "id": "` + versionID + `",
          "attributes": { "tags": ["qux"] }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.id").Equal(versionID)
		tags := obj.Path("$.data.attributes.tags").Array()
		tags.Length().Equal(1)
		tags.First().Equal("qux")
	})

	t.Run("DownloadVersion", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Name", "downloadme-versioned").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("one")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()
		firstRev := obj.Path("$.data.meta.rev").String().NotEmpty().Raw()

		e.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte(`two`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		res := e.GET("/files/download/"+fileID+"/"+firstRev).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		res.Header("Content-Disposition").HasPrefix("inline")
		res.Header("Content-Disposition").Contains(`filename="downloadme-versioned"`)
		res.Header("Content-Type").HasPrefix("text/plain")
		res.Body().Equal("one")
	})

	t.Run("FileCreateAndDownloadByVersionID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/").
			WithQuery("Name", "direct-downloadme-versioned").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("one")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()
		firstRev := obj.Path("$.data.meta.rev").String().NotEmpty().Raw()

		// Upload a new content to the file creating a new version
		e.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte(`two`)).
			Expect().Status(200)

		obj = e.POST("/files/downloads").
			WithQuery("VersionId", fileID+"/"+firstRev).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		related := obj.Path("$.links.related").String().NotEmpty().Raw()

		// Get display url
		e.GET(related).
			Expect().Status(200).
			Header("Content-Disposition").Equal(`inline; filename="direct-downloadme-versioned"`)
	})

	t.Run("RevertVersion", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create a file
		fileID = e.POST("/files/").
			WithQuery("Name", "direct-downloadme-reverted").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("one")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Upload a new content to the file creating a new version
		e.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte(`two`)).
			Expect().Status(200)

		// Get the current file version id
		versionID := e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.relationships.old_versions.data[0].id").
			String().NotEmpty().Raw()

		// Revert the last version
		e.POST("/files/revert/"+versionID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Check that the file have the reverted content
		res := e.GET("/files/download/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		res.Header("Content-Disposition").HasPrefix("inline")
		res.Header("Content-Disposition").Contains(`filename="direct-downloadme-reverted"`)
		res.Header("Content-Type").HasPrefix("text/plain")
		res.Body().Equal("one")
	})

	t.Run("CleanOldVersion", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create a file
		fileID = e.POST("/files/").
			WithQuery("Name", "downloadme-toclean").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("one")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Upload a new content to the file creating a new version
		e.PUT("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "text/plain").
			WithBytes([]byte(`two`)).
			Expect().Status(200)

		// Get the current file version id
		versionID := e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.relationships.old_versions.data[0].id").
			String().NotEmpty().Raw()

		// Delete the last versionID.
		e.DELETE("/files/"+versionID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)

		// Check that the last version have been deleted and is not downloadable
		e.GET("/files/download/"+versionID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)
	})

	t.Run("CopyVersion", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Upload some file metadata
		metadataID := e.POST("/files/upload/metadata").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
            "type": "io.cozy.files.metadata",
            "attributes": {
                "category": "report",
                "label": "foo"
            }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Upload a file content linke to the previous metadata.
		fileID = e.POST("/files/").
			WithQuery("Name", "version-to-be-copied").
			WithQuery("Type", "file").
			WithQuery("MetadataID", metadataID).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("should-be-the-same-after-copy")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Push some attributes the the last version
		obj := e.POST("/files/"+fileID+"/versions").
			WithQuery("Tags", "qux").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
            "type": "io.cozy.files.metadata",
            "attributes": {
                "label": "bar"
            }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		tags := attrs.Value("tags").Array()
		tags.Length().Equal(1)
		tags.First().Equal("qux")

		meta := attrs.Value("metadata").Object()
		meta.NotContainsKey("category")
		meta.ValueEqual("label", "bar")
	})

	t.Run("CopyVersionWithCertified", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Upload some file metadata
		metadataID := e.POST("/files/upload/metadata").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
            "type": "io.cozy.files.metadata",
            "attributes": {
                "carbonCopy": true,
                "electronicSafe": true
            }
        }
      }`)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Upload a file content linke to the previous metadata.
		fileID = e.POST("/files/").
			WithQuery("Name", "copy-version-with-certified").
			WithQuery("Type", "file").
			WithQuery("MetadataID", metadataID).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("certified carbonCopy and electronicSafe must be kept if only the qualification change")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Push some attributes the the last version
		e.POST("/files/"+fileID+"/versions").
			WithQuery("Tags", "qux").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.files.metadata",
          "attributes": { "qualification": { "purpose": "attestation" } }
        }
      }`)).
			Expect().Status(200)

		// Push overide the attributes push by the the last call.
		obj := e.POST("/files/"+fileID+"/versions").
			WithQuery("Tags", "qux").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.files.metadata",
          "attributes": { "label": "bar" }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Ensure that only the last attributres remains
		meta := obj.Path("$.data.attributes.metadata").Object()
		meta.ContainsKey("label")
		meta.NotContainsKey("qualification")
		meta.NotContainsKey("carbonCopy")
		meta.NotContainsKey("electronicSafe")
	})

	t.Run("CopyVersionWorksForNotes", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Upload a file note.
		obj := e.POST("/files/").
			WithQuery("Name", "test.cozy-note").
			WithQuery("Type", "file").
			WithHeader("Content-Type", consts.NoteMimeType).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("# Title\n\n* foo\n* bar\n")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()
		meta1 := obj.Path("$.data.attributes.metadata").Object()
		meta1.ContainsKey("title")
		meta1.ContainsKey("content")
		meta1.ContainsKey("schema")
		meta1.ContainsKey("version")

		// Update the metadatas.
		obj = e.POST("/files/"+fileID+"/versions").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
            "type": "io.cozy.files.metadata",
            "attributes": {
          "qualification": { "purpose": "attestation" }
            }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		meta := obj.Path("$.data.attributes.metadata").Object()
		meta.ValueEqual("title", meta1.Value("title").Raw())
		meta.ValueEqual("content", meta1.Value("content").Raw())
		meta.ValueEqual("schema", meta1.Value("schema").Raw())
		meta.ValueEqual("version", meta1.Value("version").Raw())
	})

	t.Run("CopyNoteWithoutImagesDatabase", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Upload a note without images.
		noteContent := "# Simple Note\n\nThis is a note without images."
		obj := e.POST("/files/").
			WithQuery("Name", "simple-note.cozy-note").
			WithQuery("Type", "file").
			WithHeader("Content-Type", consts.NoteMimeType).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(noteContent)).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()

		// Delete the io.cozy.notes.images database to simulate the case
		// where a note has never had images and the database doesn't exist yet.
		// This is the specific case that was fixed in PR #4608.
		_ = couchdb.DeleteDB(testInstance, consts.NotesImages)
		// Restore the database after the test to avoid affecting other tests.
		t.Cleanup(func() {
			_ = couchdb.CreateDB(testInstance, consts.NotesImages)
		})

		// Copy the note. This should succeed even though the images database
		// doesn't exist. Before the fix, this would fail with a database error.
		// After the fix, getImages() returns an empty slice when the database
		// doesn't exist, allowing the copy to continue.
		copyObj := e.POST("/files/"+fileID+"/copy").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		copyID := copyObj.Path("$.data.id").String().NotEmpty().NotEqual(fileID).Raw()

		// Verify the copy has the expected name.
		copyObj.Path("$.data.attributes.name").String().Equal("simple-note (copy).cozy-note")

		// Download and verify the content of the copy.
		res := e.GET("/files/download/"+copyID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		res.Header("Content-Type").Equal(consts.NoteMimeType)
		res.Body().NotEmpty()
	})

	t.Run("ArchiveNoFiles", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/archive").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "attributes": {}
        }
      }`)).
			Expect().Status(400).
			JSON().Equal("Can't create an archive with no files")
	})

	t.Run("ArchiveDirectDownload", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the dir "/archive"
		dirID := e.POST("/files/").
			WithQuery("Name", "archive").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the 3 empty files "/archive/{foo,bar,baz}"
		names := []string{"foo", "bar", "baz"}
		for _, name := range names {
			e.POST("/files/"+dirID).
				WithQuery("Name", name+".jpg").
				WithQuery("Type", "file").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(201)
		}

		// Archive several files in a single call and receive a zip file.
		e.POST("/files/archive").
			WithHeader("Content-Type", "application/zip").
			WithHeader("Accept", "application/zip").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "attributes": {
            "files": [
            "/archive/foo.jpg",
            "/archive/bar.jpg",
            "/archive/baz.jpg"
            ]
          }
        }
      }`)).
			Expect().Status(200).
			Header("Content-Type").Equal("application/zip")
	})

	t.Run("ArchiveCreateAndDownload", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the dir "/archive2"
		dirID := e.POST("/files/").
			WithQuery("Name", "archive2").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the 3 empty files "/archive/{foo,bar,baz}"
		names := []string{"foo", "bar", "baz"}
		for _, name := range names {
			e.POST("/files/"+dirID).
				WithQuery("Name", name+".jpg").
				WithQuery("Type", "file").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(201)
		}

		// Archive several files in a single call
		related := e.POST("/files/archive").
			WithHeader("Content-Type", "application/zip").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "attributes": {
            "files": [
            "/archive/foo.jpg",
            "/archive/bar.jpg",
            "/archive/baz.jpg"
            ]
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.links.related").String().NotEmpty().Raw()

		// Fetch the the file preview in order to check if it's accessible.
		e.GET(related).
			Expect().Status(200).
			Header("Content-Disposition").Equal(`attachment; filename="archive.zip"`)
	})

	t.Run("FileCreateAndDownloadByPath", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/todownload2steps"
		e.POST("/files/").
			WithQuery("Name", "todownload2steps").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201)

		// Start the download in two steps by requiring the url
		related := e.POST("/files/downloads").
			WithQuery("Path", "/todownload2steps").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.links.related").String().NotEmpty().Raw()

		// Fetch the display url.
		e.GET(related).
			Expect().Status(200).
			Header("Content-Disposition").Equal(`inline; filename="todownload2steps"`)

		// Fetch the display url with the Dl query returning an attachment instead of an inline file.
		e.GET(related).
			WithQuery("Dl", "1").
			Expect().Status(200).
			Header("Content-Disposition").Equal(`attachment; filename="todownload2steps"`)
	})

	t.Run("FileCreateAndDownloadByID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/todownload2steps"
		fileID = e.POST("/files/").
			WithQuery("Name", "todownload2stepsbis").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Start the download in two steps by requiring the url
		related := e.POST("/files/downloads").
			WithQuery("Id", fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "").
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.links.related").String().NotEmpty().Raw()

		// Fetch the display url.
		e.GET(related).
			Expect().Status(200).
			Header("Content-Disposition").Equal(`inline; filename="todownload2stepsbis"`)
	})

	t.Run("EncryptedFileCreate", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/todownload2steps"
		obj := e.POST("/files/").
			WithQuery("Name", "encryptedfile").
			WithQuery("Type", "file").
			WithQuery("Encrypted", true).
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("name", "encryptedfile")
		attrs.ValueEqual("encrypted", true)
	})

	t.Run("HeadDirOrFileNotFound", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.HEAD("/files/fakeid").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)
	})

	t.Run("HeadDirOrFileExists", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the dir "/hellothere"
		dirID := e.POST("/files/").
			WithQuery("Name", "hellothere").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		e.HEAD("/files/"+dirID).
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
	})

	t.Run("ArchiveNotFound", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/files/archive").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "attributes": {
            "files": [
              "/archive/foo.jpg",
              "/no/such/file",
              "/archive/baz.jpg"
            ]
          }
        }
      }`)).
			Expect().Status(404)
	})

	t.Run("DirTrash", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the dir "/totrashdir"
		dirID := e.POST("/files/").
			WithQuery("Name", "totrashdir").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the file "/totrashdir/child1"
		e.POST("/files/"+dirID).
			WithQuery("Name", "child1").
			WithQuery("Type", "file").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		// Create the file "/totrashdir/child2"
		e.POST("/files/"+dirID).
			WithQuery("Name", "child2").
			WithQuery("Type", "file").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		// Trash "/totrashdir"
		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Get "/totrashdir"
		e.GET("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Get "/totrashdir/child1" from the trash
		e.GET("/files/download").
			WithQuery("Path", vfs.TrashDirName+"/totrashdir/child1").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Get "/totrashdir/child2" from the trash
		e.GET("/files/download").
			WithQuery("Path", vfs.TrashDirName+"/totrashdir/child2").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Trash again "/totrashdir" and fail
		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400)
	})

	t.Run("FileTrash", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/totrashfile"
		fileID = e.POST("/files/").
			WithQuery("Name", "totrashfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the file
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Get the file from the trash
		e.GET("/files/download").
			WithQuery("Path", vfs.TrashDirName+"/totrashfile").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Create the file "/totrashfile2"
		obj := e.POST("/files/").
			WithQuery("Name", "totrashfile2").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fileID = obj.Path("$.data.id").String().NotEmpty().Raw()
		rev := obj.Path("$.data.meta.rev").String().NotEmpty().Raw()

		// Trash the file with an invalid rev
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("If-Match", "badrev"). // Invalid
			Expect().Status(412)

		// Trash the file with a valid rev
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("If-Match", rev).
			Expect().Status(200)

		// Try to trash an the already trashed file
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400)
	})

	t.Run("FileTrashAttribution", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		publicName := expectedTrashDisplayName(t, testInstance)

		fileID = e.POST("/files/").
			WithQuery("Name", "totrashwithattribution").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		obj := e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.trashed").Boolean().True()
		fcm := obj.Path("$.data.attributes.cozyMetadata").Object()
		fcm.Value("trashedAt").String().DateTime(time.RFC3339)
		trashedBy := fcm.Value("trashedBy").Object()
		trashedBy.Value("kind").String().IsEqual(vfs.TrashedByKindMember)
		trashedBy.Value("displayName").String().IsEqual(publicName)
		trashedBy.Value("domain").String().IsEqual(testInstance.Domain)
	})

	t.Run("PatchMoveToTrashAttribution", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		publicName := expectedTrashDisplayName(t, testInstance)

		fileID = e.POST("/files/").
			WithQuery("Name", "patch-trash-attribution").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		obj := e.PATCH("/files/"+fileID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "file",
          "id": "` + fileID + `",
          "attributes": { "move_to_trash": true }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		obj.Path("$.data.attributes.trashed").Boolean().True()
		fcm := obj.Path("$.data.attributes.cozyMetadata").Object()
		fcm.Value("trashedAt").String().DateTime(time.RFC3339)
		trashedBy := fcm.Value("trashedBy").Object()
		trashedBy.Value("kind").String().IsEqual(vfs.TrashedByKindMember)
		trashedBy.Value("displayName").String().IsEqual(publicName)
		trashedBy.Value("domain").String().IsEqual(testInstance.Domain)
	})

	t.Run("DirTrashAttributionTopLevelOnly", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		publicName := expectedTrashDisplayName(t, testInstance)

		dirID := e.POST("/files/").
			WithQuery("Name", "dir-trash-attribution").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		childID := e.POST("/files/"+dirID).
			WithQuery("Name", "child.txt").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		obj := e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		fcm := obj.Path("$.data.attributes.cozyMetadata").Object()
		fcm.Value("trashedAt").String().DateTime(time.RFC3339)
		trashedBy := fcm.Value("trashedBy").Object()
		trashedBy.Value("kind").String().IsEqual(vfs.TrashedByKindMember)
		trashedBy.Value("displayName").String().IsEqual(publicName)
		trashedBy.Value("domain").String().IsEqual(testInstance.Domain)

		child := e.GET("/files/"+childID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()
		child.Path("$.data.attributes.trashed").Boolean().True()
		childMeta := child.Path("$.data.attributes.cozyMetadata").Object()
		childMeta.NotContainsKey("trashedAt")
		childMeta.NotContainsKey("trashedBy")
	})

	t.Run("ForbidMovingTrashedFile", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/forbidmovingtrashedfile"
		fileID = e.POST("/files/").
			WithQuery("Name", "forbidmovingtrashedfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the file
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Try to patch a trashed file.
		e.PATCH("/files/"+fileID).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "type": "file",
          "id": "` + fileID + `",
          "attributes": { "dir_id": "` + consts.RootDirID + `" }
        }
      }`)).
			Expect().Status(400)
	})

	t.Run("FileRestore", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/torestorefile"
		fileID = e.POST("/files/").
			WithQuery("Name", "torestorefile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the file
		obj := e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()
		obj.Path("$.data.attributes.trashed").Boolean().True()
		trashedAt := obj.Path("$.data.attributes.cozyMetadata.trashedAt").String().NotEmpty().Raw()
		trashedBy := obj.Path("$.data.attributes.cozyMetadata.trashedBy").Object()
		trashedByKind := trashedBy.Value("kind").String().NotEmpty().Raw()
		trashedByDisplayName := trashedBy.Value("displayName").String().NotEmpty().Raw()
		trashedByDomain := trashedBy.Value("domain").String().NotEmpty().Raw()

		// Restore the file
		obj = e.POST("/files/trash/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()
		obj.Path("$.data.attributes.trashed").Boolean().False()
		fcm := obj.Path("$.data.attributes.cozyMetadata").Object()
		fcm.Value("trashedAt").String().IsEqual(trashedAt)
		restoredBy := fcm.Value("trashedBy").Object()
		restoredBy.Value("kind").String().IsEqual(trashedByKind)
		restoredBy.Value("displayName").String().IsEqual(trashedByDisplayName)
		restoredBy.Value("domain").String().IsEqual(trashedByDomain)

		// Download the file.
		e.GET("/files/download").
			WithQuery("Path", "/torestorefile").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
	})

	t.Run("FileRestoreWithConflicts", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/torestorefilewithconflict"
		fileID = e.POST("/files/").
			WithQuery("Name", "torestorefilewithconflict").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the file
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Create a new file "/torestorefilewithconflict" while the old one is
		// in the trash
		e.POST("/files/").
			WithQuery("Name", "torestorefilewithconflict").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201)

		// Restore the trashed file
		obj := e.POST("/files/trash/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// The file should be restore but with a new name.
		obj.Path("$.data.id").Equal(fileID)
		attrs := obj.Path("$.data.attributes").Object()
		attrs.Value("name").String().HasPrefix("torestorefilewithconflict")
		attrs.Value("name").String().NotEqual("torestorefilewithconflict")
	})

	t.Run("FileRestoreWithWithoutParent", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the dir "/torestorein"
		dirID := e.POST("/files/").
			WithQuery("Name", "torestorein").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the file "/torestorein/torestorefilewithconflict"
		fileID = e.POST("/files/"+dirID).
			WithQuery("Name", "torestorefilewithconflict").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the file
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Trash the dir
		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Restore the file without the dir
		obj := e.POST("/files/trash/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("name", "torestorefilewithconflict")
		attrs.NotHasValue("dir_id", consts.RootDirID)
	})

	t.Run("FileRestoreWithWithoutParent2", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the dir "/torestorein2"
		dirID := e.POST("/files/").
			WithQuery("Name", "torestorein2").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the file "/torestorein2/torestorefilewithconflict2"
		fileID = e.POST("/files/"+dirID).
			WithQuery("Name", "torestorefilewithconflict2").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the dir
		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Restore the file deleted with the dir
		obj := e.POST("/files/trash/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("name", "torestorefilewithconflict2")
		attrs.NotHasValue("dir_id", consts.RootDirID)
	})

	t.Run("DirRestore", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the dir "/torestorein3"
		dirID := e.POST("/files/").
			WithQuery("Name", "torestorein3").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the file "/torestorein3/totrashfile"
		fileID = e.POST("/files/"+dirID).
			WithQuery("Name", "totrashfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the dir
		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Get that the file is marked as trashed.
		e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.trashed").Boolean().True()

		// Restore the dir.
		e.POST("/files/trash/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Get that the file is not marked as trashed.
		e.GET("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.trashed").Boolean().False()
	})

	t.Run("DirRestoreWithConflicts", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the dir "/torestoredirwithconflict"
		dirID := e.POST("/files/").
			WithQuery("Name", "torestoredirwithconflict").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the dir
		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Create a new dir with the same name
		e.POST("/files/").
			WithQuery("Name", "torestoredirwithconflict").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201)

		// Restore the deleted dir
		obj := e.POST("/files/trash/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.Value("name").String().HasPrefix("torestoredirwithconflict")
		attrs.Value("name").String().NotEqual("torestoredirwithconflict")
	})

	t.Run("TrashList", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/tolistfile"
		fileID = e.POST("/files/").
			WithQuery("Name", "tolistfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the dir "/tolistdir"
		dirID := e.POST("/files/").
			WithQuery("Name", "tolistdir").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the dir
		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Trash the file
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Get the number of elements in trash
		e.GET("/files/trash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Value("data").Array().Length().Gt(2)
	})

	t.Run("TrashClear", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Check that the trash is not empty
		e.GET("/files/trash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Value("data").Array().Length().Gt(2)

		// Empty the trash
		e.DELETE("/files/trash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)

		// Check that the trash is empty
		e.GET("/files/trash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Value("data").Array().Length().Equal(0)
	})

	t.Run("DestroyFile", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the file "/tolistfile"
		fileID = e.POST("/files/").
			WithQuery("Name", "tolistfile").
			WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Content-MD5", "UmfjCVWct/albVkURcJJfg==").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("foo,bar")).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create the dir "/tolistdir"
		dirID := e.POST("/files/").
			WithQuery("Name", "tolistdir").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Trash the dir
		e.DELETE("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Trash the file
		e.DELETE("/files/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Destroy the file
		e.DELETE("/files/trash/"+fileID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)

		// List the elements in trash
		e.GET("/files/trash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Value("data").Array().Length().Equal(1)

		// Detroy the dir
		e.DELETE("/files/trash/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)

		// List the elements in trash, should be empty
		e.GET("/files/trash").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Value("data").Array().Length().Equal(0)
	})

	t.Run("ThumbnailImages", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Get the image file infos.
		obj := e.GET("/files/"+imgID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Retrieve the thumbnail links
		links := obj.Path("$.data.links").Object()
		large := links.Value("large").String().NotEmpty().Raw()
		medium := links.Value("medium").String().NotEmpty().Raw()
		small := links.Value("small").String().NotEmpty().Raw()
		tiny := links.Value("tiny").String().NotEmpty().Raw()

		// Test the large thumbnail
		e.GET(large).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Header("Content-Type").Equal("image/jpeg")

		// Test the medium thumbnail
		e.GET(medium).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Header("Content-Type").Equal("image/jpeg")

		// Test the small thumbnail
		e.GET(small).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Header("Content-Type").Equal("image/jpeg")

		// Test the tiny thumbnail
		e.GET(tiny).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Header("Content-Type").Equal("image/jpeg")
	})

	t.Run("ThumbnailPDFs", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		rawPDF, err := os.ReadFile("../../tests/fixtures/dev-desktop.pdf")
		require.NoError(t, err)

		// Upload a PDF file
		pdfID := e.POST("/files/").
			WithQuery("Name", "dev-desktop.pdf").
			WithQuery("Type", "file").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/pdf").
			WithBytes(rawPDF).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().
			Path("$.data.id").String().NotEmpty().Raw()

		// Get the PDF file
		obj := e.GET("/files/"+pdfID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Retrieve the thumbnail links
		links := obj.Path("$.data.links").Object()
		large := links.Value("large").String().NotEmpty().Raw()
		medium := links.Value("medium").String().NotEmpty().Raw()
		small := links.Value("small").String().NotEmpty().Raw()

		// Wait for tiny thumbnail generation
		time.Sleep(time.Second)

		tiny := links.Value("tiny").String().NotEmpty().Raw()

		// Large, medium, and small are not generated automatically
		e.GET(large).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404).
			Header("Content-Type").Equal("image/png")
		e.GET(medium).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404).
			Header("Content-Type").Equal("image/png")
		e.GET(small).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404).
			Header("Content-Type").Equal("image/png")

		// Wait for tiny thumbnail generation
		time.Sleep(1 * time.Second)

		// Test the tiny thumbnail
		e.GET(tiny).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Header("Content-Type").Equal("image/jpeg")

		// Wait for other thumbnails generation
		time.Sleep(3 * time.Second)

		e.GET(large).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Header("Content-Type").Equal("image/jpeg")
		e.GET(medium).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Header("Content-Type").Equal("image/jpeg")
		e.GET(small).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Header("Content-Type").Equal("image/jpeg")
	})

	t.Run("GetFileByPublicLink", func(t *testing.T) {
		var publicToken string
		var err error

		t.Run("success", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			// Upload a file
			fileID = e.POST("/files/").
				WithQuery("Name", "publicfile").
				WithQuery("Type", "file").
				WithHeader("Content-Type", "application/pdf").
				WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
				WithHeader("Authorization", "Bearer "+token).
				WithBytes([]byte("foo")).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().
				Path("$.data.id").String().NotEmpty().Raw()

			// Generating a new token
			publicToken, err = testInstance.MakeJWT(consts.ShareAudience, "email", "io.cozy.files", "", time.Now())
			require.NoError(t, err)

			expires := time.Now().Add(2 * time.Minute)
			rules := permission.Set{
				permission.Rule{
					Type:   "io.cozy.files",
					Verbs:  permission.Verbs(permission.GET),
					Values: []string{fileID},
				},
			}
			perms := permission.Permission{
				Permissions: rules,
			}
			_, err = permission.CreateShareSet(testInstance, &permission.Permission{Type: "app", Permissions: rules}, "", map[string]string{"email": publicToken}, nil, perms, &expires, false)
			require.NoError(t, err)

			// Use the public token to get the file
			e.GET("/files/"+fileID).
				WithHeader("Authorization", "Bearer "+publicToken).
				Expect().Status(200)
		})

		t.Run("GetFileByPublicLinkRateExceeded", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			// Blocking the file by accessing it a lot of times
			for i := 0; i < 1999; i++ {
				err = config.GetRateLimiter().CheckRateLimitKey(fileID, limits.SharingPublicLinkType)
				assert.NoError(t, err)
			}

			err = config.GetRateLimiter().CheckRateLimitKey(fileID, limits.SharingPublicLinkType)
			require.Error(t, err)
			require.ErrorIs(t, err, limits.ErrRateLimitReached)

			e.GET("/files/"+fileID).
				WithHeader("Authorization", "Bearer "+publicToken).
				Expect().Status(500).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().Path("$.errors[0].detail").String().Equal("Rate limit exceeded")
		})
	})

	t.Run("GetAllDocs", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/files/_all_docs").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
				"keys": ["io.cozy.files.root-dir", "%s"]
      }`, fileID))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().IsEqual(2)

		elem := data.Value(0).Object()
		attrs := elem.Value("attributes").Object()
		attrs.HasValue("path", "/")

		elem = data.Value(1).Object()
		attrs = elem.Value("attributes").Object()
		attrs.Value("path").String().NotEmpty()
		links := elem.Value("links").Object()
		links.Value("tiny").String().NotEmpty()
	})

	t.Run("Find", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		type M map[string]interface{}
		type S []interface{}

		defIndex := M{"index": M{"fields": S{"_id"}}}
		_, err := couchdb.DefineIndexRaw(testInstance, "io.cozy.files", &defIndex)
		assert.NoError(t, err)

		defIndex2 := M{"index": M{"fields": S{"type"}}}
		_, err = couchdb.DefineIndexRaw(testInstance, "io.cozy.files", &defIndex2)
		assert.NoError(t, err)

		obj := e.POST("/files/_find").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "selector": {
          "type": "file"
        },
        "limit": 1
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		meta := obj.Value("meta").Object()
		meta.NotEmpty()
		meta.NotContainsKey("execution_stats")

		data := obj.Value("data").Array()
		data.Length().Equal(1)

		attrs := data.First().Object().Value("attributes").Object()
		attrs.Value("name").String().NotEmpty()
		attrs.Value("type").String().NotEmpty()
		attrs.Value("size").String().AsNumber(10)
		attrs.Value("path").String().NotEmpty()

		obj = e.POST("/files/_find").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
				"selector": {
					"_id": {
						"$gt": null
					}
				},
				"limit": 1,
				"execution_stats": true
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Array()
		data.Length().Equal(1)

		nextURL, err := url.Parse(obj.Path("$.links.next").String().NotEmpty().Raw())
		require.NoError(t, err)

		e.POST(nextURL.Path).
			WithQueryString(nextURL.RawQuery).
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
				"selector": {
					"_id": {
						"$gt": null
					}
				},
				"limit": 1,
				"execution_stats": true
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Array()
		data.Length().Equal(1)

		// Make a request with a whitelist of fields
		obj = e.POST("/files/_find").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
				"selector": {
					"_id": {
						"$gt": null
					}
				},
				"fields": ["dir_id", "name", "name"],
				"limit": 1
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs = obj.Path("$.data[0].attributes").Object()
		attrs.Value("name").String().NotEmpty()
		attrs.Value("dir_id").String().NotEmpty()
		attrs.Value("type").String().NotEmpty()
		attrs.NotContainsKey("path")
		attrs.NotContainsKey("create_at")
		attrs.NotContainsKey("updated_at")
		attrs.NotContainsKey("tags")

		obj = e.POST("/files/_find").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
				"selector": {
					"type": "file"
				},
				"fields": ["name"],
				"limit": 1
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Array()
		data.Length().Equal(1)

		attrs = data.First().Object().Value("attributes").Object()
		attrs.Value("name").String().NotEmpty()
		attrs.Value("type").String().NotEmpty()
		attrs.Value("size").String().AsNumber(10)
		attrs.ValueEqual("trashed", false)
		attrs.NotContainsKey("encrypted")
		attrs.NotContainsKey("created_at")
		attrs.NotContainsKey("updated_at")
		attrs.NotContainsKey("tags")
		attrs.NotContainsKey("executable")
		attrs.NotContainsKey("dir_id")
		attrs.NotContainsKey("path")

		obj = e.POST("/files/_find").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
				"selector": {
					"type": "file"
				},
				"fields": ["type", "path"],
				"limit": 1
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Array()
		data.Length().Equal(1)

		attrs = data.First().Object().Value("attributes").Object()
		attrs.Value("path").String().NotEmpty()
		attrs.Value("type").String().NotEmpty()
		attrs.NotContainsKey("name")
		attrs.NotContainsKey("created_at")
		attrs.NotContainsKey("updated_at")
		attrs.NotContainsKey("tags")
		attrs.NotContainsKey("executable")

		// Create dir "/aDirectoryWithReferencedBy"
		dirID := e.POST("/files/").
			WithQuery("Name", "aDirectoryWithReferencedBy").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Create a reference
		e.POST("/files/"+dirID+"/relationships/referenced_by").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
        "data": {
          "id": "fooalbumid",
          "type": "io.cozy.photos.albums"
        }
      }`)).
			Expect().Status(200)

		obj = e.POST("/files/_find").
			WithHeader("Content-Type", "application/json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{
				"selector": {
					"name": "aDirectoryWithReferencedBy"
				},
				"limit": 1
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Array()
		data.Length().Equal(1)

		elem := data.First().Object()

		attrs = elem.Value("attributes").Object()
		attrs.ValueEqual("name", "aDirectoryWithReferencedBy")
		attrs.ContainsKey("referenced_by")

		dataRefs := elem.Path("$.relationships.referenced_by.data").Array()
		dataRefs.Length().Equal(1)
		ref := dataRefs.First().Object()
		ref.ValueEqual("id", "fooalbumid")
		ref.ValueEqual("type", "io.cozy.photos.albums")
	})

	t.Run("UploadToMagicFolder", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		upload := func(name string) *httpexpect.Object {
			return e.POST("/files/").
				WithQuery("Type", "file").
				WithQuery("Name", name).
				WithQuery("MagicFolder", "io.cozy.apps/mail").
				WithHeader("Content-Type", "text/plain").
				WithHeader("Authorization", "Bearer "+token).
				WithBytes([]byte("baz")).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()
		}

		// The first upload creates the folder at the root
		dirID := upload("first.txt").Path("$.data.attributes.dir_id").String().NotEmpty().Raw()

		obj := e.GET("/files/"+dirID).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()
		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("name", "Mail")
		attrs.ValueEqual("dir_id", consts.RootDirID)
		ref := obj.Path("$.data.relationships.referenced_by.data").Array().First().Object()
		ref.ValueEqual("id", "io.cozy.apps/mail")
		ref.ValueEqual("type", consts.Apps)

		// The next uploads reuse the folder, even after it has been renamed
		upload("second.txt").Path("$.data.attributes.dir_id").String().Equal(dirID)

		e.PATCH("/files/"+dirID).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`{"data":{"type":"io.cozy.files","id":"` + dirID + `","attributes":{"name":"Attachments"}}}`)).
			Expect().Status(200)

		upload("third.txt").Path("$.data.attributes.dir_id").String().Equal(dirID)

		// Only known references are accepted
		e.POST("/files/").
			WithQuery("Type", "file").
			WithQuery("Name", "unknown.txt").
			WithQuery("MagicFolder", "io.cozy.apps/unknown").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(422)

		// MagicFolder cannot be combined with a dir-id
		e.POST("/files/"+dirID).
			WithQuery("Type", "file").
			WithQuery("Name", "both.txt").
			WithQuery("MagicFolder", "io.cozy.apps/mail").
			WithHeader("Content-Type", "text/plain").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("baz")).
			Expect().Status(422)
	})

	t.Run("DirSize", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		parentID := e.POST("/files/").
			WithQuery("Name", "dirsizeparent").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		subID := e.POST("/files/"+parentID).
			WithQuery("Name", "dirsizesub").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		subsubID := e.POST("/files/"+subID).
			WithQuery("Name", "dirsizesub").
			WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.id").String().NotEmpty().Raw()

		// Upload files into each folder 10 times.
		for i := 0; i < 10; i++ {
			name := "file" + strconv.Itoa(i)

			e.POST("/files/"+parentID).
				WithQuery("Name", name).
				WithQuery("Type", "file").
				WithHeader("Content-Type", "text/plain").
				WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
				WithHeader("Authorization", "Bearer "+token).
				WithBytes([]byte("foo")).
				Expect().Status(201)

			e.POST("/files/"+subID).
				WithQuery("Name", name).
				WithQuery("Type", "file").
				WithHeader("Content-Type", "text/plain").
				WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
				WithHeader("Authorization", "Bearer "+token).
				WithBytes([]byte("foo")).
				Expect().Status(201)

			e.POST("/files/"+subsubID).
				WithQuery("Name", name).
				WithQuery("Type", "file").
				WithHeader("Content-Type", "text/plain").
				WithHeader("Content-MD5", "rL0Y20zC+Fzt72VPzMSk2A==").
				WithHeader("Authorization", "Bearer "+token).
				WithBytes([]byte("foo")).
				Expect().Status(201)
		}

		// validate the subsub dir
		obj := e.GET("/files/"+subsubID+"/size").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.ValueEqual("type", consts.DirSizes)
		data.ValueEqual("id", subsubID)
		data.Value("attributes").Object().ValueEqual("size", "30")

		// validate the sub dir
		obj = e.GET("/files/"+subID+"/size").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Object()
		data.ValueEqual("type", consts.DirSizes)
		data.ValueEqual("id", subID)
		data.Value("attributes").Object().ValueEqual("size", "60")

		// validate the parent dir
		obj = e.GET("/files/"+parentID+"/size").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Object()
		data.ValueEqual("type", consts.DirSizes)
		data.ValueEqual("id", parentID)
		data.Value("attributes").Object().ValueEqual("size", "90")
	})
}

func readFile(fs vfs.VFS, name string) ([]byte, error) {
	doc, err := fs.FileByPath(name)
	if err != nil {
		return nil, err
	}
	f, err := fs.OpenFile(doc)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func loadLocale() error {
	locale := consts.DefaultLocale
	assetsPath := config.GetConfig().Assets
	if assetsPath != "" {
		pofile := path.Join("../..", assetsPath, "locales", locale+".po")
		po, err := os.ReadFile(pofile)
		if err != nil {
			return fmt.Errorf("Can't load the po file for %s", locale)
		}
		i18n.LoadLocale(locale, "", po)
	}
	return nil
}

func TestFileCreationTriggersAntivirusScan(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	// Enable antivirus in context
	conf := config.GetConfig()
	if conf.Contexts == nil {
		conf.Contexts = make(map[string]interface{})
	}
	conf.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
		"antivirus": map[string]interface{}{
			"enabled": true,
			"address": "localhost:3310",
		},
	}
	t.Cleanup(func() {
		delete(conf.Contexts, config.DefaultInstanceContext)
	})

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{})

	// Create file via VFS
	fs := inst.VFS()
	parent, err := fs.DirByPath("/")
	require.NoError(t, err)

	content := []byte("test content for antivirus scan")
	doc, err := vfs.NewFileDoc("test-av.txt", parent.DocID, int64(len(content)), nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)

	file, err := fs.CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = file.Write(content)
	require.NoError(t, err)
	err = file.Close()
	require.NoError(t, err)

	// Wait for event trigger to process (best-effort, may take a moment)
	testutils.WaitForOrFail(t, 2*time.Second, func() bool {
		updatedDoc, err := fs.FileByID(doc.DocID)
		if err != nil {
			return false
		}
		return updatedDoc.AntivirusScan != nil &&
			updatedDoc.AntivirusScan.Status == vfs.AVStatusPending
	})

	// Verify antivirus status is pending
	updatedDoc, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	require.NotNil(t, updatedDoc.AntivirusScan)
	assert.Equal(t, vfs.AVStatusPending, updatedDoc.AntivirusScan.Status)

	// Cleanup
	_ = fs.DestroyFile(updatedDoc)
}
