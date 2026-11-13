package files

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
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

// recentsSetup returns an instance whose recents are already seeded, so that
// the first page of a test only holds the rows that the test stored.
func recentsSetup(t *testing.T) *recentsEnv {
	t.Helper()
	env := recentsSetupUnseeded(t)
	recents.Seed(env.inst)
	return env
}

func recentsSetupUnseeded(t *testing.T) *recentsEnv {
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

// createRow stores a crafted row, for an item that may not exist.
func (env *recentsEnv) createRow(t *testing.T, id, date string) *recents.FileDirActivity {
	t.Helper()
	row := &recents.FileDirActivity{
		DocID:          id,
		LastEditedAt:   date,
		LastActivityAt: date,
		Name:           "name-of-" + id,
		Type:           consts.FileType,
		Mime:           "image/jpeg",
		Class:          "image",
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(env.inst, row))
	return row
}

// createFile stores an image without content under a chosen ID, and its row.
func (env *recentsEnv) createFile(t *testing.T, id, dirID, date string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(id+".jpg", dirID, 1234, nil, "image/jpeg", "image", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	doc.SetID(id)
	require.NoError(t, env.inst.VFS().CreateNamedFileDoc(doc))
	env.createRow(t, id, date)
	return doc
}

// createRootFiles stores files at the root, with their rows at the same date.
func (env *recentsEnv) createRootFiles(t *testing.T, date string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		env.createFile(t, id, consts.RootDirID, date)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// countFilesRequests returns the number of requests that fn sends to the
// databases of the files and of the recents. The other requests of a call
// (token, permissions, flags) do not depend on the page.
func countFilesRequests(t *testing.T, fn func()) int {
	t.Helper()
	client := config.CouchClient()
	original := client.Transport
	defer func() { client.Transport = original }()
	var count atomic.Int32
	client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "io-cozy-files") {
			count.Add(1)
		}
		return original.RoundTrip(r)
	})
	fn()
	return int(count.Load())
}

// idsOf returns the IDs of the "data" or of the "included" member of a page,
// in their order. A missing member has none.
func idsOf(page *httpexpect.Object, key string) []string {
	ids := make([]string, 0)
	if docs, ok := page.Raw()[key].([]interface{}); ok {
		for _, doc := range docs {
			ids = append(ids, doc.(map[string]interface{})["id"].(string))
		}
	}
	return ids
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

func TestRecentsListPagination(t *testing.T) {
	env := recentsSetup(t)

	// The rows are stored in an order unrelated to the sort.
	env.createRootFiles(t, "2026-11-05T10:00:00Z", "id-c", "id-a", "id-e")
	env.createRootFiles(t, "2026-11-04T10:00:00Z", "id-g", "id-f")
	env.createRootFiles(t, "2026-11-05T10:00:00Z", "id-b", "id-d")

	t.Run("the requests to CouchDB do not grow with the rows", func(t *testing.T) {
		// The files are in the same folder: the path of a file asks for its
		// parent folder, once per folder.
		var one, all *httpexpect.Object
		small := countFilesRequests(t, func() { one = env.getPage(t, "page[limit]=1") })
		large := countFilesRequests(t, func() { all = env.getPage(t, "page[limit]=7") })
		assert.Len(t, idsOf(one, "included"), 1)
		assert.Len(t, idsOf(all, "included"), 7)
		// One request for the marker of the seed, one for the rows, and one
		// for their files.
		assert.Equal(t, 3, small)
		assert.Equal(t, 3, large)

		// A full page that nothing follows has no next link.
		all.Path("$.meta.count").Number().IsEqual(7)
		all.NotContainsKey("links")
	})

	t.Run("traverses same-date rows over several pages", func(t *testing.T) {
		// The next link keeps the page size, and its cursor is the position
		// of the last row of the page.
		first := env.getPage(t, "page[limit]=2")
		assert.Equal(t, []string{"id-e", "id-d"}, idsOf(first, "data"))
		second := env.followNext(t, first)
		assert.Equal(t, []string{"id-c", "id-b"}, idsOf(second, "data"))
		third := env.followNext(t, second)
		assert.Equal(t, []string{"id-a", "id-g"}, idsOf(third, "data"))
		last := env.followNext(t, third)
		assert.Equal(t, []string{"id-f"}, idsOf(last, "data"))
		last.NotContainsKey("links")
	})

	t.Run("a cursor alone resumes with the default page size", func(t *testing.T) {
		cursor, err := recents.Position{LastActivityAt: "2026-11-05T10:00:00Z", DocID: "id-b"}.Cursor()
		require.NoError(t, err)
		page := env.getPage(t, url.Values{"page[cursor]": {cursor}}.Encode())
		assert.Equal(t, []string{"id-a", "id-g", "id-f"}, idsOf(page, "data"))
	})

	t.Run("the response is compressed for a client that accepts it", func(t *testing.T) {
		res := env.e(t).GET("/files/recents").
			WithHeader("Authorization", "Bearer "+env.token).
			WithHeader("Accept-Encoding", "gzip").
			Expect().Status(200)
		res.Header("Content-Encoding").IsEqual("gzip")
		res.Header("Vary").Contains("Accept-Encoding")
	})
}

func TestRecentsListPageSizes(t *testing.T) {
	env := recentsSetup(t)

	for i := 0; i < 101; i++ {
		env.createRootFiles(t, "2026-11-05T10:00:00Z", fmt.Sprintf("id-%03d", i))
	}

	t.Run("default", func(t *testing.T) {
		page := env.getPage(t, "")
		assert.Len(t, idsOf(page, "data"), 50)
		page.Path("$.links.next").String().NotEmpty()
	})

	t.Run("above the maximum", func(t *testing.T) {
		page := env.getPage(t, "page[limit]=101")
		assert.Len(t, idsOf(page, "data"), 100)

		next, err := url.Parse(page.Path("$.links.next").String().Raw())
		require.NoError(t, err)
		assert.Equal(t, "100", next.Query().Get("page[limit]"))
	})
}

func TestRecentsListCompletesTheItems(t *testing.T) {
	env := recentsSetup(t)
	fs := env.inst.VFS()

	// Two rows without a document come first: one whose file was destroyed,
	// and one whose file never existed.
	require.NoError(t, fs.DeleteFileDoc(env.createFile(t, "gone-2", consts.RootDirID, "2026-11-09T10:00:00Z")))
	env.createRow(t, "gone-1", "2026-11-09T10:00:00Z")

	dir, err := vfs.Mkdir(fs, "/Holidays", nil)
	require.NoError(t, err)
	env.createFile(t, "cat", dir.ID(), "2026-11-08T10:00:00Z")
	env.createRow(t, dir.ID(), "2026-11-07T10:00:00Z")
	// A file without content cannot go through vfs.TrashFile.
	trashed := env.createFile(t, "trashed", consts.TrashDirID, "2026-11-06T10:00:00Z")
	trashed.Trashed = true
	require.NoError(t, couchdb.UpdateDoc(env.inst, trashed))
	foreign := &recents.FileDirActivity{
		DocID:          "foreign",
		OwnerDomain:    "no-such-owner.example.net",
		LastViewedAt:   "2026-11-05T10:00:00Z",
		LastActivityAt: "2026-11-05T10:00:00Z",
		Name:           "report.txt",
		Type:           consts.FileType,
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(env.inst, foreign))

	whole := env.getPage(t, "")
	fileRef := map[string]interface{}{"type": consts.Files, "id": "cat"}

	t.Run("the items without a document are left out", func(t *testing.T) {
		// The item of another instance has a resource, and no document.
		assert.Equal(t, []string{"cat", dir.ID(), "trashed", "foreign"}, idsOf(whole, "data"))
		whole.Path("$.meta.count").Number().IsEqual(4)
		assert.Equal(t, []string{"cat", dir.ID(), "trashed"}, idsOf(whole, "included"))

		// A page of such rows is empty, and still leads to the next rows: its
		// cursor is the position of the last row read.
		empty := env.getPage(t, "page[limit]=2")
		empty.Value("data").Array().IsEmpty()
		empty.Path("$.meta.count").Number().IsEqual(0)
		empty.NotContainsKey("included")
		assert.Equal(t, []string{"cat", dir.ID()}, idsOf(env.followNext(t, empty), "data"))
	})

	t.Run("a resource holds its row and points to its file", func(t *testing.T) {
		var row recents.FileDirActivity
		require.NoError(t, couchdb.GetDoc(env.inst, consts.FilesRecents, "cat", &row))
		assert.Equal(t, map[string]interface{}{
			"type": "io.cozy.files.recents",
			"id":   "cat",
			"attributes": map[string]interface{}{
				"last_edited_at":   "2026-11-08T10:00:00Z",
				"last_activity_at": "2026-11-08T10:00:00Z",
				"name":             "name-of-cat",
				"type":             "file",
				"mime":             "image/jpeg",
				"class":            "image",
			},
			"meta":          map[string]interface{}{"rev": row.DocRev},
			"relationships": map[string]interface{}{"file": map[string]interface{}{"data": fileRef}},
		}, whole.Path("$.data[0]").Object().Raw())
	})

	t.Run("the items are included as the files routes give them", func(t *testing.T) {
		file := whole.Path("$.included[0]").Object()
		file.Value("type").String().IsEqual(consts.Files)
		file.Path("$.attributes.path").String().IsEqual("/Holidays/cat.jpg")
		file.Path("$.links.small").String().Contains("/files/cat/thumbnails/")

		folder := whole.Path("$.included[1]").Object()
		folder.Path("$.attributes.type").String().IsEqual(consts.DirType)
		folder.Path("$.attributes.path").String().IsEqual("/Holidays")

		// A trashed file is still given.
		whole.Path("$.included[2].attributes.trashed").Boolean().IsTrue()
	})

	t.Run("an item of another instance has an empty file relationship", func(t *testing.T) {
		resource := whole.Path("$.data[3]").Object()
		resource.Path("$.attributes.owner_domain").String().IsEqual(foreign.OwnerDomain)
		resource.Path("$.relationships.file").Object().ContainsKey("data")
		resource.Path("$.relationships.file.data").IsNull()
	})
}

func TestRecentsListChecksTheRequest(t *testing.T) {
	env := recentsSetup(t)
	env.createRootFiles(t, "2026-11-05T10:00:00Z", "id-a")

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
				assert.Equal(t, []string{"id-a"}, idsOf(page, "data"))
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

	perm := permission.Permission{Permissions: rules}
	for _, tc := range []struct {
		name, subject, permID string
		createSet             func(code string) error
	}{
		{"share-interact", "interact:" + s.SID + ":" + memberEmail, permission.ShareInteractPermissionID(s.SID), func(code string) error {
			_, err := permission.CreateShareInteractSet(env.inst, s.SID, map[string]string{memberEmail: code}, perm)
			return err
		}},
		{"share-preview", "preview:" + s.SID + ":" + memberEmail, "", func(code string) error {
			_, err := permission.CreateSharePreviewSet(env.inst, s.SID, map[string]string{memberEmail: code}, nil, perm)
			return err
		}},
		{"share-by-link", "email", "", func(code string) error {
			_, err := permission.CreateShareSet(env.inst,
				&permission.Permission{Type: permission.TypeWebapp, Permissions: rules},
				"", map[string]string{"email": code}, nil, perm, nil, false)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := env.inst.CreateShareCode(tc.subject, tc.permID)
			require.NoError(t, err)
			require.NoError(t, tc.createSet(code))
			env.e(t).GET("/files/recents").
				WithHeader("Authorization", "Bearer "+code).
				Expect().Status(403).
				Body().NotContains("id-a")
		})
	}

	// The other invalid cursors are in TestParseCursorRejectsAnInvalidCursor.
	for _, tc := range []struct{ param, value string }{
		{"page[limit]", ""},
		{"page[limit]", "abc"},
		{"page[limit]", "0"},
		{"page[limit]", "-1"},
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
	})
}

func TestRecentsListOfAMissingDatabase(t *testing.T) {
	env := recentsSetup(t)
	require.NoError(t, couchdb.DeleteDB(env.inst, consts.FilesRecents))
	cursor, err := recents.Position{LastActivityAt: "2026-11-05T10:00:00Z", DocID: "id-a"}.Cursor()
	require.NoError(t, err)

	for _, query := range []string{"", url.Values{"page[cursor]": {cursor}}.Encode()} {
		page := env.getPage(t, query)
		page.Value("data").Array().IsEmpty()
		page.NotContainsKey("links")
	}
}

func TestRecentsSeedOnFirstCall(t *testing.T) {
	env := recentsSetupUnseeded(t)

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
	// activity, with its document.
	page := env.getPage(t, "")
	assert.Contains(t, idsOf(page, "data"), before.ID())
	assert.Contains(t, idsOf(page, "included"), before.ID())

	// The seed runs once: a file created after the first page is not seeded.
	after := seedable("after-first-call.txt")
	page = env.getPage(t, "")
	assert.NotContains(t, idsOf(page, "data"), after.ID())
}
