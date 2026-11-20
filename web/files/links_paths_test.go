package files

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicLinkPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	require.NoError(t, loadLocale(), "Could not load default locale translations")
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	config.GetConfig().Fs.URL = &url.URL{Scheme: "file", Host: "localhost", Path: t.TempDir()}

	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient(consts.Files)
	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/files": Routes,
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	client := func(t *testing.T) *httpexpect.Expect { return testutils.CreateTestClient(t, ts.URL) }
	jsonAPI := httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}
	mkdir := func(t *testing.T, parent, name string) string {
		return client(t).POST("/files/"+parent).
			WithQuery("Name", name).WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(http.StatusCreated).JSON(jsonAPI).
			Object().Path("$.data.id").String().NotEmpty().Raw()
	}

	privateID := mkdir(t, "", "Private")
	clientsID := mkdir(t, privateID, "Clients")
	docsID := mkdir(t, clientsID, "Docs")
	subID := mkdir(t, docsID, "Sub")
	otherID := mkdir(t, clientsID, "Other")

	link := func(name string, values []string, verbs ...permission.Verb) string {
		code, err := inst.MakeJWT(consts.ShareAudience, name, "io.cozy.files", "", time.Now())
		require.NoError(t, err)
		rules := permission.Set{permission.Rule{
			Type:   consts.Files,
			Verbs:  permission.VerbSet{},
			Values: values,
		}}
		for _, v := range verbs {
			rules[0].Verbs[v] = struct{}{}
		}
		_, err = permission.CreateShareSet(inst, &permission.Permission{Type: "app", Permissions: rules},
			"", map[string]string{name: code}, nil, permission.Permission{Permissions: rules}, nil, false)
		require.NoError(t, err)
		return code
	}
	readLink := link("read", []string{docsID}, permission.GET)
	writeLink := link("write", []string{docsID}, permission.GET, permission.POST, permission.PATCH, permission.DELETE)

	noAncestorName := func(t *testing.T, res *httpexpect.Response) *httpexpect.Object {
		t.Helper()
		assert.NotContains(t, res.Body().Raw(), "Private")
		assert.NotContains(t, res.Body().Raw(), "Clients")
		return res.JSON(jsonAPI).Object()
	}
	get := func(t *testing.T, code, url string, query map[string]string) *httpexpect.Response {
		t.Helper()
		req := client(t).GET(url).WithHeader("Authorization", "Bearer "+code)
		for k, v := range query {
			req = req.WithQuery(k, v)
		}
		return req.Expect()
	}

	t.Run("ByID", func(t *testing.T) {
		obj := noAncestorName(t, get(t, readLink, "/files/"+docsID, nil).Status(http.StatusOK))
		obj.Path("$.data.attributes.path").String().IsEqual("/Docs")
		for _, inc := range obj.Value("included").Array().Iter() {
			inc.Object().Path("$.attributes.path").String().IsEqual("/Docs/Sub")
		}
	})

	t.Run("SubfolderByID", func(t *testing.T) {
		obj := noAncestorName(t, get(t, readLink, "/files/"+subID, nil).Status(http.StatusOK))
		obj.Path("$.data.attributes.path").String().IsEqual("/Docs/Sub")
	})

	t.Run("Children", func(t *testing.T) {
		obj := noAncestorName(t, get(t, readLink, "/files/"+docsID+"/relationships/contents", nil).Status(http.StatusOK))
		obj.Path("$.data[0].attributes.path").String().IsEqual("/Docs/Sub")
	})

	t.Run("ByPath", func(t *testing.T) {
		obj := noAncestorName(t, get(t, readLink, "/files/metadata", map[string]string{"Path": "/Docs/Sub"}).Status(http.StatusOK))
		obj.Path("$.data.id").String().IsEqual(subID)
		obj.Path("$.data.attributes.path").String().IsEqual("/Docs/Sub")
	})

	t.Run("ReuseFilePath", func(t *testing.T) {
		e := client(t)
		id := e.POST("/files/"+subID).WithQuery("Name", "report.txt").WithQuery("Type", "file").
			WithHeader("Content-Type", "text/plain").WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte("report contents")).Expect().Status(http.StatusCreated).
			JSON(jsonAPI).Object().Path("$.data.id").String().Raw()
		folder := noAncestorName(t, get(t, readLink, "/files/"+subID, nil).Status(http.StatusOK))
		p := folder.Path("$.data.attributes.path").String().IsEqual("/Docs/Sub").Raw() + "/" +
			folder.Path("$.included[0].attributes.name").String().IsEqual("report.txt").Raw()

		get(t, readLink, "/files/download", map[string]string{"Path": p}).
			Status(http.StatusOK).Body().IsEqual("report contents")
		download := e.POST("/files/downloads").WithQuery("Path", p).
			WithHeader("Authorization", "Bearer "+readLink).Expect().Status(http.StatusOK)
		link := noAncestorName(t, download).Path("$.links.related").String().Raw()
		e.GET(link).Expect().Status(http.StatusOK).Body().IsEqual("report contents")

		req := e.POST("/files/archive").WithHeader("Authorization", "Bearer "+readLink).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{"data":{"attributes":{"files":[%q]}}}`, p)))
		archive := noAncestorName(t, req.Expect().Status(http.StatusOK))
		archive.Path("$.data.attributes.files[0]").String().IsEqual(p)
		e.GET(archive.Path("$.links.related").String().Raw()).Expect().Status(http.StatusOK).
			Header("Content-Type").IsEqual("application/zip")

		patch := func(code, path string) *httpexpect.Response {
			return e.PATCH("/files/metadata").WithQuery("Path", path).
				WithHeader("Authorization", "Bearer "+code).WithHeader("Content-Type", "application/vnd.api+json").
				WithBytes([]byte(`{"data":{"attributes":{"tags":["reviewed"]}}}`)).Expect()
		}
		patch(readLink, p).Status(http.StatusForbidden)
		noAncestorName(t, patch(writeLink, p).Status(http.StatusOK)).Path("$.data.id").String().IsEqual(id)

		ownerPath := "/Private/Clients" + p
		get(t, readLink, "/files/download", map[string]string{"Path": ownerPath}).Status(http.StatusNotFound)
		e.POST("/files/downloads").WithQuery("Path", ownerPath).WithQuery("Id", id).
			WithHeader("Authorization", "Bearer "+readLink).Expect().Status(http.StatusNotFound)
		e.POST("/files/archive").WithHeader("Authorization", "Bearer "+readLink).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{"data":{"attributes":{"files":[%q,%q]}}}`, p, ownerPath))).
			Expect().Status(http.StatusNotFound)
		patch(writeLink, ownerPath).Status(http.StatusNotFound)
	})

	t.Run("ByPathOutsideTheLinkAnswersLikeAMissingPath", func(t *testing.T) {
		missing := get(t, readLink, "/files/metadata", map[string]string{"Path": "/Docs/Missing"}).
			Status(http.StatusNotFound).Body().Raw()
		for _, p := range []string{"/Private/Clients/Docs", "/Private", "/Docs/../Other", "/Clients/Other", "/Other"} {
			body := get(t, readLink, "/files/metadata", map[string]string{"Path": p}).
				Status(http.StatusNotFound).Body().Raw()
			assert.Equal(t, missing, body, p)
		}
	})

	t.Run("ByPathWithTwoRoots", func(t *testing.T) {
		both := link("two-roots", []string{docsID, otherID}, permission.GET)
		for _, id := range []string{docsID, otherID} {
			obj := noAncestorName(t, get(t, both, "/files/"+id, nil).Status(http.StatusOK))
			p := obj.Path("$.data.attributes.path").String().NotEmpty().Raw()
			byPath := get(t, both, "/files/metadata", map[string]string{"Path": p}).Status(http.StatusOK)
			noAncestorName(t, byPath).Path("$.data.id").String().IsEqual(id)
		}
	})

	t.Run("ByPathInSameNamedFolders", func(t *testing.T) {
		otherDocsID := mkdir(t, otherID, "Docs")
		mkdir(t, otherDocsID, "Sub")
		both := link("both", []string{otherDocsID, docsID}, permission.GET)
		for _, p := range []string{"/Docs", "/Docs/Sub"} {
			get(t, both, "/files/metadata", map[string]string{"Path": p}).Status(http.StatusNotFound)
		}
	})

	t.Run("TargetGone", func(t *testing.T) {
		gone := link("gone", []string{"no-such-file"}, permission.GET)
		get(t, gone, "/files/metadata", map[string]string{"Path": "/Private"}).Status(http.StatusNotFound)
	})

	t.Run("OwnerKeepsTheAbsolutePath", func(t *testing.T) {
		get(t, token, "/files/"+docsID, nil).Status(http.StatusOK).JSON(jsonAPI).
			Object().Path("$.data.attributes.path").String().IsEqual("/Private/Clients/Docs")
	})

	t.Run("CreatedFolder", func(t *testing.T) {
		res := client(t).POST("/files/"+docsID).
			WithQuery("Name", "New").WithQuery("Type", "directory").
			WithHeader("Authorization", "Bearer "+writeLink).
			Expect().Status(http.StatusCreated)
		noAncestorName(t, res).Path("$.data.attributes.path").String().IsEqual("/Docs/New")
	})

	t.Run("RestorePath", func(t *testing.T) {
		res := client(t).DELETE("/files/"+subID).
			WithHeader("Authorization", "Bearer "+writeLink).
			Expect().Status(http.StatusOK)
		noAncestorName(t, res).Path("$.data.attributes.restore_path").String().IsEqual("/Docs")
	})
}
