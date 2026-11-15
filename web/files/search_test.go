package files

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/search"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestSearchFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	osURL := os.Getenv("COZY_TEST_OPENSEARCH_URL")
	if osURL != "" {
		t.Setenv("COZY_SEARCH_OPENSEARCH_URL", osURL)
		t.Setenv("COZY_SEARCH_OPENSEARCH_INDEX_PREFIX", fmt.Sprintf("test-%d", time.Now().UnixNano()))
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	_, tok := setup.GetTestClient(consts.Files)
	_, contactsTok := setup.GetTestClient(consts.Contacts)
	ts := setup.GetTestServer("/files", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	post := func(t *testing.T, token, path string, body map[string]any) *httpexpect.Response {
		e := testutils.CreateTestClient(t, ts.URL)
		path, query, _ := strings.Cut(path, "?")
		return e.POST(path).
			WithQueryString(query).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "application/vnd.api+json").
			WithJSON(body).
			Expect()
	}

	t.Run("q is required and limited", func(t *testing.T) {
		post(t, tok, "/files/search", map[string]any{"q": "   "}).Status(400)
		post(t, tok, "/files/search", map[string]any{}).Status(400)
		post(t, tok, "/files/search", map[string]any{"q": strings.Repeat("é", 257)}).Status(400)
	})

	t.Run("a permission on the files is required", func(t *testing.T) {
		post(t, contactsTok, "/files/search", map[string]any{"q": "report"}).Status(403)
	})

	t.Run("search not configured", func(t *testing.T) {
		saved := config.GetConfig().Search
		config.GetConfig().Search = nil
		t.Cleanup(func() { config.GetConfig().Search = saved })
		post(t, tok, "/files/search", map[string]any{"q": "report"}).Status(503)
	})

	if osURL == "" {
		t.Log("COZY_TEST_OPENSEARCH_URL is not set: the search itself is not tested")
		return
	}

	client := config.GetConfig().Search
	ctx := context.Background()
	dir, err := vfs.MkdirAll(inst.VFS(), "/Documents/2026")
	require.NoError(t, err)
	for _, name := range []string{"Quarterly report.pdf", "Annual report.pdf"} {
		doc, err := vfs.NewFileDoc(name, dir.ID(), 5, nil, "application/pdf", "pdf", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		f, err := inst.VFS().CreateFile(doc, nil)
		require.NoError(t, err)
		_, err = f.Write([]byte("hello"))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	require.NoError(t, search.Index(ctx, inst, logger.WithNamespace("search-test")))
	require.Eventually(t, func() bool {
		res, err := search.Search(ctx, client, inst, "report", 0, "")
		return err == nil && len(res.Hits) == 2
	}, 5*time.Second, 200*time.Millisecond)

	t.Run("search", func(t *testing.T) {
		obj := post(t, tok, "/files/search", map[string]any{"q": "quarterly report"}).
			Status(200).JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
		data := obj.Value("data").Array()
		data.Length().IsEqual(2)
		first := data.Value(0).Object()
		first.HasValue("type", consts.Files)
		attrs := first.Value("attributes").Object()
		attrs.HasValue("name", "Quarterly report.pdf")
		attrs.HasValue("type", "file")
		attrs.HasValue("mime", "application/pdf")
		attrs.HasValue("class", "pdf")
		attrs.HasValue("dir_id", dir.ID())
		attrs.HasValue("path", "/Documents/2026")
		attrs.NotContainsKey("drive_id")
		attrs.Value("updated_at").String().NotEmpty()
		obj.Value("links").Object().NotContainsKey("next")
	})

	t.Run("pages", func(t *testing.T) {
		obj := post(t, tok, "/files/search", map[string]any{"q": "report", "limit": 1}).
			Status(200).JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
		obj.Value("data").Array().Length().IsEqual(1)
		next := obj.Value("links").Object().Value("next").String().HasPrefix("/files/search?page[cursor]=").Raw()
		firstID := obj.Value("data").Array().Value(0).Object().Value("id").String().Raw()

		second := post(t, tok, next, map[string]any{"q": "report", "limit": 1}).
			Status(200).JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
		second.Value("data").Array().Length().IsEqual(1)
		second.Value("data").Array().Value(0).Object().Value("id").String().NotEqual(firstID)
	})

	t.Run("invalid cursor", func(t *testing.T) {
		post(t, tok, "/files/search", map[string]any{"q": "report", "cursor": "nope"}).Status(400)
	})
}
