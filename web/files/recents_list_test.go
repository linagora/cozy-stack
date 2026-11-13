package files

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const recentsFlag = "drive.recents.enabled"

// recentsEnv is an instance with the recents enabled, and a server of the
// files routes.
type recentsEnv struct {
	inst   *instance.Instance
	client *oauth.Client
	url    string
	token  string
}

// e returns an HTTP client that reports its failures on the running test.
func (env *recentsEnv) e(t *testing.T) *httpexpect.Expect {
	t.Helper()
	return testutils.CreateTestClient(t, env.url)
}

func recentsSetup(t *testing.T) *recentsEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.URL = &url.URL{
		Scheme: "file",
		Host:   "localhost",
		Path:   t.TempDir(),
	}

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	client, token := setup.GetTestClient(consts.Files)
	ts := setup.GetTestServer("/files", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	testutils.WithFlag(t, inst, recentsFlag, true)

	return &recentsEnv{
		inst:   inst,
		client: client,
		url:    ts.URL,
		token:  token,
	}
}

// createRow stores a crafted row, as recording does not exist yet.
func (env *recentsEnv) createRow(t *testing.T, id, date string) *recents.FileDirActivity {
	t.Helper()
	row := &recents.FileDirActivity{
		DocID:          id,
		LastEditedAt:   date,
		LastActivityAt: date,
		Name:           "name-of-" + id,
		Type:           consts.FileType,
		UpdatedAt:      date,
		DirID:          consts.RootDirID,
		Path:           "/name-of-" + id,
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(env.inst, row))
	return row
}

// moveRow gives a stored row a new date.
func (env *recentsEnv) moveRow(t *testing.T, id, date string) {
	t.Helper()
	var row recents.FileDirActivity
	require.NoError(t, couchdb.GetDoc(env.inst, consts.FilesRecents, id, &row))
	row.LastActivityAt = date
	require.NoError(t, couchdb.UpdateDoc(env.inst, &row))
}

// getPage calls the endpoint with a query string, and returns its document.
func (env *recentsEnv) getPage(t *testing.T, query string) *httpexpect.Object {
	t.Helper()
	return env.e(t).GET("/files/recents").
		WithQueryString(query).
		WithHeader("Authorization", "Bearer "+env.token).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
}

// followNext requests the page of the links.next of a document.
func (env *recentsEnv) followNext(t *testing.T, page *httpexpect.Object) *httpexpect.Object {
	t.Helper()
	next, err := url.Parse(page.Path("$.links.next").String().Raw())
	require.NoError(t, err)
	assert.Equal(t, "/files/recents", next.Path)
	return env.getPage(t, next.RawQuery)
}

// pageIDs returns the IDs of the resources of a page, in their order.
func pageIDs(page *httpexpect.Object) []string {
	data := page.Value("data").Array()
	ids := make([]string, 0)
	for _, item := range data.Iter() {
		ids = append(ids, item.Object().Value("id").String().Raw())
	}
	return ids
}

func assertNoNextLink(page *httpexpect.Object) {
	page.NotContainsKey("links")
}

func TestRecentsListPagination(t *testing.T) {
	env := recentsSetup(t)

	// The rows are stored in an order unrelated to the sort.
	env.createRow(t, "id-c", "2026-11-05T10:00:00Z")
	env.createRow(t, "id-g", "2026-11-04T10:00:00Z")
	env.createRow(t, "id-a", "2026-11-05T10:00:00Z")
	env.createRow(t, "id-e", "2026-11-05T10:00:00Z")
	env.createRow(t, "id-f", "2026-11-04T10:00:00Z")
	env.createRow(t, "id-b", "2026-11-05T10:00:00Z")
	env.createRow(t, "id-d", "2026-11-05T10:00:00Z")

	t.Run("traverses same-date rows over several pages", func(t *testing.T) {
		first := env.getPage(t, "page[limit]=2")
		assert.Equal(t, []string{"id-e", "id-d"}, pageIDs(first))

		// The next link keeps the page size, and its cursor is the position
		// of the last row of the page.
		next, err := url.Parse(first.Path("$.links.next").String().Raw())
		require.NoError(t, err)
		assert.Equal(t, "2", next.Query().Get("page[limit]"))
		raw, err := base64.RawURLEncoding.DecodeString(next.Query().Get("page[cursor]"))
		require.NoError(t, err)
		assert.JSONEq(t, `{"last_activity_at": "2026-11-05T10:00:00Z", "id": "id-d"}`, string(raw))

		second := env.followNext(t, first)
		assert.Equal(t, []string{"id-c", "id-b"}, pageIDs(second))

		third := env.followNext(t, second)
		assert.Equal(t, []string{"id-a", "id-g"}, pageIDs(third))

		last := env.followNext(t, third)
		assert.Equal(t, []string{"id-f"}, pageIDs(last))
		assertNoNextLink(last)
	})

	t.Run("no next link when the last page is full", func(t *testing.T) {
		first := env.getPage(t, "page[limit]=4")
		assert.Equal(t, []string{"id-e", "id-d", "id-c", "id-b"}, pageIDs(first))

		last := env.followNext(t, first)
		assert.Equal(t, []string{"id-a", "id-g", "id-f"}, pageIDs(last))
		assertNoNextLink(last)

		whole := env.getPage(t, "page[limit]=7")
		assert.Len(t, pageIDs(whole), 7)
		whole.Path("$.meta.count").Number().IsEqual(7)
		assertNoNextLink(whole)
	})

	t.Run("a cursor alone resumes with the default page size", func(t *testing.T) {
		cursor, err := recents.Position{LastActivityAt: "2026-11-05T10:00:00Z", DocID: "id-b"}.Cursor()
		require.NoError(t, err)
		page := env.getPage(t, url.Values{"page[cursor]": {cursor}}.Encode())
		assert.Equal(t, []string{"id-a", "id-g", "id-f"}, pageIDs(page))
	})

	// The last subtest: it changes the order of the rows.
	t.Run("resumes after the row of the cursor moved ahead", func(t *testing.T) {
		first := env.getPage(t, "page[limit]=4")
		assert.Equal(t, []string{"id-e", "id-d", "id-c", "id-b"}, pageIDs(first))

		env.moveRow(t, "id-b", "2026-11-06T10:00:00Z")

		last := env.followNext(t, first)
		assert.Equal(t, []string{"id-a", "id-g", "id-f"}, pageIDs(last))
		assertNoNextLink(last)
	})
}

func TestRecentsListPageSizes(t *testing.T) {
	env := recentsSetup(t)

	for i := 0; i < 101; i++ {
		env.createRow(t, fmt.Sprintf("id-%03d", i), "2026-11-05T10:00:00Z")
	}

	t.Run("default", func(t *testing.T) {
		page := env.getPage(t, "")
		assert.Len(t, pageIDs(page), 50)
		page.Path("$.links.next").String().NotEmpty()
	})

	t.Run("above the maximum", func(t *testing.T) {
		page := env.getPage(t, "page[limit]=101")
		assert.Len(t, pageIDs(page), 100)

		next, err := url.Parse(page.Path("$.links.next").String().Raw())
		require.NoError(t, err)
		assert.Equal(t, "100", next.Query().Get("page[limit]"))

		last := env.getPage(t, next.RawQuery)
		assert.Equal(t, []string{"id-000"}, pageIDs(last))
		assertNoNextLink(last)
	})
}

func TestRecentsListReturnsStoredMetadata(t *testing.T) {
	env := recentsSetup(t)

	t.Run("the row of a file that does not exist", func(t *testing.T) {
		row := &recents.FileDirActivity{
			DocID:          "019a6f2e-7c1d-7e3a-9b42-000000000001",
			LastEditedAt:   "2026-11-05T09:00:00Z",
			LastViewedAt:   "2026-11-05T10:00:00Z",
			LastActivityAt: "2026-11-05T10:00:00Z",
			Name:           "report.txt",
			Type:           consts.FileType,
			Mime:           "text/plain",
			Class:          "text",
			Size:           "1234",
			UpdatedAt:      "2026-11-05T09:00:00Z",
			DirID:          "019a6f2e-7c1d-7e3a-9b42-000000000002",
			Path:           "/Reports/report.txt",
			SchemaVersion:  recents.SchemaVersion,
		}
		require.NoError(t, couchdb.CreateNamedDocWithDB(env.inst, row))

		page := env.getPage(t, "")

		data := page.Value("data").Array()
		data.Length().IsEqual(1)
		assert.Equal(t, map[string]interface{}{
			"type": "io.cozy.files.recents",
			"id":   row.DocID,
			"attributes": map[string]interface{}{
				"last_edited_at":   "2026-11-05T09:00:00Z",
				"last_viewed_at":   "2026-11-05T10:00:00Z",
				"last_activity_at": "2026-11-05T10:00:00Z",
				"name":             "report.txt",
				"type":             "file",
				"mime":             "text/plain",
				"class":            "text",
				"size":             "1234",
				"updated_at":       "2026-11-05T09:00:00Z",
				"dir_id":           "019a6f2e-7c1d-7e3a-9b42-000000000002",
				"path":             "/Reports/report.txt",
			},
			"meta": map[string]interface{}{"rev": row.DocRev},
		}, data.Value(0).Object().Raw())
		require.NoError(t, couchdb.DeleteDoc(env.inst, row))
	})

	t.Run("the row of an item of a shared drive", func(t *testing.T) {
		row := &recents.FileDirActivity{
			DocID:          "019a6f2e-7c1d-7e3a-9b42-000000000003",
			DriveID:        "019a6f2e-0000-7000-8000-000000000001",
			OwnerDomain:    "no-such-owner.example.net",
			LastViewedAt:   "2026-11-05T10:00:00Z",
			LastActivityAt: "2026-11-05T10:00:00Z",
			Name:           "report.txt",
			Type:           consts.FileType,
			UpdatedAt:      "2026-11-05T09:00:00Z",
			DirID:          "019a6f2e-7c1d-7e3a-9b42-000000000004",
			Path:           "/Reports/report.txt",
			SchemaVersion:  recents.SchemaVersion,
		}
		require.NoError(t, couchdb.CreateNamedDocWithDB(env.inst, row))

		resource := env.getPage(t, "").Path("$.data[0]").Object()
		resource.Value("id").String().IsEqual(row.DocID)
		attrs := resource.Value("attributes").Object()
		attrs.Value("driveId").String().IsEqual(row.DriveID)
		attrs.Value("owner_domain").String().IsEqual(row.OwnerDomain)
		attrs.Value("path").String().IsEqual(row.Path)
		attrs.NotContainsKey("last_edited_at")
		require.NoError(t, couchdb.DeleteDoc(env.inst, row))
	})

	t.Run("the row of an item is not refreshed from the item", func(t *testing.T) {
		dir, err := vfs.Mkdir(env.inst.VFS(), "/real-name", nil)
		require.NoError(t, err)

		row := env.createRow(t, dir.ID(), "2026-11-05T10:00:00Z")

		attrs := env.getPage(t, "").Path("$.data[0].attributes").Object()
		attrs.Value("name").String().IsEqual(row.Name)
		attrs.Value("type").String().IsEqual(consts.FileType)
		attrs.Value("path").String().IsEqual(row.Path)
		attrs.NotContainsKey("schema_version")

		// The route of the list does not shadow the route of an item.
		env.e(t).GET("/files/"+dir.ID()).
			WithHeader("Authorization", "Bearer "+env.token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.data.attributes.name").String().IsEqual("real-name")
	})
}

func TestRecentsListChecksTheRequest(t *testing.T) {
	env := recentsSetup(t)
	env.createRow(t, "id-a", "2026-11-05T10:00:00Z")

	t.Run("no token", func(t *testing.T) {
		env.e(t).GET("/files/recents").Expect().Status(401)
	})

	for _, tc := range []struct {
		name   string
		scope  string
		status int
	}{
		{"another doctype", "io.cozy.events", 403},
		{"the rows only", consts.FilesRecents, 403},
		{"not the GET verb", consts.Files + ":POST", 403},
		{"not the whole set", consts.Files + ":GET:" + consts.RootDirID, 403},
		{"all the verbs on the doctype", consts.Files, 200},
		{"the GET verb on the doctype", consts.Files + ":GET", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := env.inst.MakeJWT(consts.AccessTokenAudience, env.client.ClientID, tc.scope, "", time.Now())
			require.NoError(t, err)
			res := env.e(t).GET("/files/recents").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(tc.status)
			if tc.status == 200 {
				page := res.JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
				assert.Equal(t, []string{"id-a"}, pageIDs(page))
			}
		})
	}

	// The codes have a permission on the whole doctype: they are rejected for
	// what they are, not for their rules.
	const memberEmail = "bob@example.net"
	rules := permission.Set{permission.Rule{
		Type:  consts.Files,
		Verbs: permission.Verbs(permission.GET),
	}}

	s := &sharing.Sharing{
		Active: true,
		Owner:  true,
		Drive:  true,
		Members: []sharing.Member{
			{Status: sharing.MemberStatusOwner, Name: "Alice"},
			{Status: sharing.MemberStatusReady, Name: "Bob", Email: memberEmail},
		},
	}
	require.NoError(t, couchdb.CreateDoc(env.inst, s))

	expectRejected := func(t *testing.T, code string) {
		t.Helper()
		env.e(t).GET("/files/recents").
			WithHeader("Authorization", "Bearer "+code).
			Expect().Status(403).
			Body().NotContains("id-a")
	}

	t.Run("share-interact", func(t *testing.T) {
		code, err := env.inst.CreateShareCode("interact:"+s.SID+":"+memberEmail, permission.ShareInteractPermissionID(s.SID))
		require.NoError(t, err)
		_, err = permission.CreateShareInteractSet(env.inst, s.SID,
			map[string]string{memberEmail: code},
			permission.Permission{Permissions: rules})
		require.NoError(t, err)

		expectRejected(t, code)
	})

	t.Run("share-preview", func(t *testing.T) {
		code, err := env.inst.CreateShareCode("preview:"+s.SID+":"+memberEmail, "")
		require.NoError(t, err)
		_, err = permission.CreateSharePreviewSet(env.inst, s.SID,
			map[string]string{memberEmail: code}, nil,
			permission.Permission{Permissions: rules})
		require.NoError(t, err)

		expectRejected(t, code)
	})

	t.Run("share-by-link", func(t *testing.T) {
		code, err := env.inst.CreateShareCode("email", "")
		require.NoError(t, err)
		_, err = permission.CreateShareSet(env.inst,
			&permission.Permission{Type: permission.TypeWebapp, Permissions: rules},
			"", map[string]string{"email": code}, nil,
			permission.Permission{Permissions: rules}, nil, false)
		require.NoError(t, err)

		expectRejected(t, code)
	})

	// The other invalid cursors are in TestParseCursorRejectsAnInvalidCursor.
	for _, tc := range []struct{ param, value string }{
		{"page[limit]", ""},
		{"page[limit]", "abc"},
		{"page[limit]", "0"},
		{"page[limit]", "-1"},
		{"page[limit]", "1.5"},
		{"page[limit]", "99999999999999999999"},
		{"page[cursor]", ""},
		{"page[cursor]", "not base64!"},
	} {
		t.Run(tc.param+" "+tc.value, func(t *testing.T) {
			env.e(t).GET("/files/recents").
				WithQuery(tc.param, tc.value).
				WithHeader("Authorization", "Bearer "+env.token).
				Expect().Status(422).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().Path("$.errors[0].source.parameter").String().IsEqual(tc.param)
		})
	}

	t.Run("flag off", func(t *testing.T) {
		testutils.WithFlag(t, env.inst, recentsFlag, false)

		env.e(t).GET("/files/recents").
			WithHeader("Authorization", "Bearer "+env.token).
			Expect().Status(404)

		// The flag comes first: the route does not exist, whatever the request.
		env.e(t).GET("/files/recents").Expect().Status(404)
		env.e(t).GET("/files/recents").WithQuery("page[limit]", "nope").
			WithHeader("Authorization", "Bearer "+env.token).
			Expect().Status(404)
	})
}

func TestRecentsListWithoutRows(t *testing.T) {
	env := recentsSetup(t)

	t.Run("empty database", func(t *testing.T) {
		page := env.getPage(t, "")
		page.Value("data").Array().IsEmpty()
		page.Path("$.meta.count").Number().IsEqual(0)
		assertNoNextLink(page)
	})

	t.Run("missing database", func(t *testing.T) {
		require.NoError(t, couchdb.DeleteDB(env.inst, consts.FilesRecents))
		cursor, err := recents.Position{LastActivityAt: "2026-11-05T10:00:00Z", DocID: "id-a"}.Cursor()
		require.NoError(t, err)

		for _, query := range []string{"", url.Values{"page[cursor]": {cursor}}.Encode()} {
			page := env.getPage(t, query)
			page.Value("data").Array().IsEmpty()
			assertNoNextLink(page)
		}
	})
}

func TestRecentsSeedOnFirstCall(t *testing.T) {
	env := recentsSetup(t)

	seedable := func(name string) *vfs.FileDoc {
		t.Helper()
		doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
			"text/plain", "text", time.Now().Add(-time.Hour), false, false, false, nil)
		require.NoError(t, err)
		f, err := env.inst.VFS().CreateFile(doc, nil)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return doc
	}

	before := seedable("before-first-call.txt")

	// The first page seeds the recents: the file appears without any recorded
	// activity.
	page := env.getPage(t, "")
	assert.Contains(t, pageIDs(page), before.ID())

	// The seed runs once: a file created after the first page is not seeded.
	after := seedable("after-first-call.txt")
	page = env.getPage(t, "")
	assert.NotContains(t, pageIDs(page), after.ID())
}
