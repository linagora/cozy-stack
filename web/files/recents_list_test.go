package files

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gavv/httpexpect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markRecentsSeeded suppresses the lazy seed and drops the rows it
// materializes for the instance tree, so that the crafted rows below are the
// only rows of the traversal.
func markRecentsSeeded(t *testing.T, inst *instance.Instance) {
	t.Helper()
	require.NoError(t, recents.Seed(inst))
	for _, row := range recentsRows(t, inst) {
		row := row
		require.NoError(t, couchdb.DeleteDoc(inst, &row))
	}
}

// assertNoNext asserts the traversal is over.
func assertNoNext(t *testing.T, body *httpexpect.Value) {
	t.Helper()
	body.Object().NotContainsKey("links")
}

// insertRecentsRow writes a crafted recents row: the test controls the sort
// date and the resource key.
func insertRecentsRow(t *testing.T, inst *instance.Instance, key, source, date string) {
	t.Helper()
	row := &recents.FileDirActivity{
		DocID:          key,
		SourceLocator:  source,
		LastActivityAt: date,
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, row))
}

func findRowByKey(t *testing.T, inst *instance.Instance, key string) *recents.FileDirActivity {
	t.Helper()
	var row recents.FileDirActivity
	err := couchdb.GetDoc(inst, consts.FilesRecents, key, &row)
	if couchdb.IsNotFoundError(err) {
		return nil
	}
	require.NoError(t, err)
	return &row
}

// getRecentsPage calls the endpoint and returns the parsed document.
func getRecentsPage(t *testing.T, e *httpexpect.Expect, token, query string) *httpexpect.Value {
	t.Helper()
	req := e.GET("/files/recents").WithHeader("Authorization", "Bearer "+token)
	if query != "" {
		req = req.WithQueryString(query)
	}
	return req.Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"})
}

// recentsFileIDs returns the source file IDs of the page, in order.
func recentsFileIDs(t *testing.T, body *httpexpect.Value) []string {
	t.Helper()
	arr := body.Path("$.data").Array()
	n := int(arr.Length().Raw())
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, arr.Value(i).Object().Path("$.attributes.file_id").String().Raw())
	}
	return ids
}

// followNext requests the page pointed by links.next and returns it.
func followNext(t *testing.T, e *httpexpect.Expect, token string, body *httpexpect.Value) *httpexpect.Value {
	t.Helper()
	next := body.Path("$.links.next").String().Raw()
	u, err := url.Parse(next)
	require.NoError(t, err)
	require.NotEmpty(t, u.RawQuery, "the next link must carry the cursor")
	return e.GET(u.Path).WithQueryString(u.RawQuery).
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"})
}

// recentsSetup boots a test instance with a session, the drive app and its
// test server, and suppresses the lazy seed.
func recentsSetup(t *testing.T) (*instance.Instance, *httpexpect.Expect, string) {
	t.Helper()
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	config.GetConfig().Fs.URL = &url.URL{
		Scheme: "file",
		Host:   "localhost",
		Path:   t.TempDir(),
	}

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	sess, err := session.New(inst, session.LongRun, "")
	require.NoError(t, err)
	createRecentsApp(t, inst, "drive")
	markRecentsSeeded(t, inst)
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)

	ts := recentsTestServer(t, inst, sess)
	e := testutils.CreateTestClient(t, ts.URL)
	return inst, e, inst.BuildAppToken("drive", sess.ID())
}

func TestRecentsOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	fA := createRecentsFile(t, inst, "a.txt", "text/plain")
	fB := createRecentsFile(t, inst, "b.txt", "text/plain")
	fC := createRecentsFile(t, inst, "c.txt", "text/plain")
	fD := createRecentsFile(t, inst, "d.txt", "text/plain")
	fE := createRecentsFile(t, inst, "e.txt", "text/plain")

	src := inst.Domain + "/"
	insertRecentsRow(t, inst, "tie-1", src+fD.ID(), "2026-01-04T00:00:00Z")
	insertRecentsRow(t, inst, "tie-2", src+fE.ID(), "2026-01-04T00:00:00Z")
	insertRecentsRow(t, inst, "key-a", src+fA.ID(), "2026-01-03T00:00:00Z")
	insertRecentsRow(t, inst, "key-b", src+fB.ID(), "2026-01-02T00:00:00Z")
	insertRecentsRow(t, inst, "key-c", src+fC.ID(), "2026-01-01T00:00:00Z")

	body := getRecentsPage(t, e, token, "")

	// last_activity_at desc, then _id desc on ties
	assert.Equal(t,
		[]string{fE.ID(), fD.ID(), fA.ID(), fB.ID(), fC.ID()},
		recentsFileIDs(t, body))
	assertNoNext(t, body)
	body.Path("$.meta").Object().NotContainsKey("incomplete")
}

func TestRecentsPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	files := make([]*vfs.FileDoc, 5)
	dates := []string{
		"2026-01-05T00:00:00Z", "2026-01-04T00:00:00Z", "2026-01-03T00:00:00Z",
		"2026-01-02T00:00:00Z", "2026-01-01T00:00:00Z",
	}
	for i := range files {
		files[i] = createRecentsFile(t, inst, "p"+string(rune('0'+i))+".txt", "text/plain")
		insertRecentsRow(t, inst, "page-"+string(rune('0'+i)),
			inst.Domain+"/"+files[i].ID(), dates[i])
	}

	body := getRecentsPage(t, e, token, "page[limit]=2")
	assert.Equal(t, []string{files[0].ID(), files[1].ID()}, recentsFileIDs(t, body))

	body = followNext(t, e, token, body)
	assert.Equal(t, []string{files[2].ID(), files[3].ID()}, recentsFileIDs(t, body))

	body = followNext(t, e, token, body)
	assert.Equal(t, []string{files[4].ID()}, recentsFileIDs(t, body))
	assertNoNext(t, body)
}

// nextCursor returns the decoded cursor of the links.next of a page.
func nextCursor(t *testing.T, body *httpexpect.Value) string {
	t.Helper()
	u, err := url.Parse(body.Path("$.links.next").String().Raw())
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(u.Query().Get("cursor"))
	require.NoError(t, err)
	return string(raw)
}

func TestRecentsOpaqueIDsAndCursor(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	// Three rows, two of them sharing the same date, keyed as in production:
	// the key holds the internal ID of the source instance.
	files := make([]*vfs.FileDoc, 3)
	for i := range files {
		files[i] = createRecentsFile(t, inst, fmt.Sprintf("opaque-%d.txt", i), "text/plain")
	}
	insertRecentsRow(t, inst, recents.ResourceKey(inst, files[0].ID()),
		inst.Domain+"/"+files[0].ID(), "2026-02-02T00:00:00Z")
	insertRecentsRow(t, inst, recents.ResourceKey(inst, files[1].ID()),
		inst.Domain+"/"+files[1].ID(), "2026-02-01T00:00:00Z")
	insertRecentsRow(t, inst, recents.ResourceKey(inst, files[2].ID()),
		inst.Domain+"/"+files[2].ID(), "2026-02-01T00:00:00Z")

	// The two same-date rows come in _id desc order.
	tied := []string{files[1].ID(), files[2].ID()}
	if recents.ResourceKey(inst, tied[0]) < recents.ResourceKey(inst, tied[1]) {
		tied[0], tied[1] = tied[1], tied[0]
	}
	expected := []string{files[0].ID(), tied[0], tied[1]}

	var seen []string
	body := getRecentsPage(t, e, token, "page[limit]=1")
	for {
		data := body.Path("$.data").Array()
		require.Equal(t, 1.0, data.Length().Raw())
		id := data.Value(0).Object().Value("id").String().Raw()
		assert.Len(t, id, 32, "the id is the hash of the row key")
		assert.NotContains(t, id, inst.DocID, "the instance ID never leaves the stack")
		seen = append(seen, data.Value(0).Object().Path("$.attributes.file_id").String().Raw())
		body.Path("$.meta").Object().NotContainsKey("incomplete")
		if len(seen) == len(expected) {
			break
		}
		assert.NotContains(t, nextCursor(t, body), inst.DocID,
			"the cursor carries the hash of the row key, not the key")
		body = followNext(t, e, token, body)
	}
	assert.Equal(t, expected, seen, "the traversal resumes at the right row among same-date rows")
	assertNoNext(t, body)
}

func TestRecentsCursorOfAChangedRow(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	fNew := createRecentsFile(t, inst, "changed-new.txt", "text/plain")
	fOld := createRecentsFile(t, inst, "changed-old.txt", "text/plain")
	src := inst.Domain + "/"
	insertRecentsRow(t, inst, "changed-new", src+fNew.ID(), "2026-03-02T00:00:00Z")
	insertRecentsRow(t, inst, "changed-old", src+fOld.ID(), "2026-03-01T00:00:00Z")

	body := getRecentsPage(t, e, token, "page[limit]=1")
	assert.Equal(t, []string{fNew.ID()}, recentsFileIDs(t, body))

	// A new activity moves the row of the cursor before the next page.
	row := findRowByKey(t, inst, "changed-new")
	require.NotNil(t, row)
	row.LastActivityAt = "2026-03-03T00:00:00Z"
	require.NoError(t, couchdb.UpdateDoc(inst, row))

	body = followNext(t, e, token, body)
	assert.Equal(t, []string{fOld.ID()}, recentsFileIDs(t, body),
		"the traversal resumes before the date of the cursor")
	body.Path("$.meta.incomplete").Boolean().IsTrue()
}

func TestRecentsSkipsAndRefills(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	fTrashed := createRecentsFile(t, inst, "trashed.txt", "text/plain")
	fOK1 := createRecentsFile(t, inst, "ok1.txt", "text/plain")
	fOK2 := createRecentsFile(t, inst, "ok2.txt", "text/plain")

	e.DELETE("/files/"+fTrashed.ID()).
		WithHeader("Authorization", "Bearer "+token).
		Expect().Status(200)

	src := inst.Domain + "/"
	insertRecentsRow(t, inst, "row-trashed", src+fTrashed.ID(), "2026-01-05T00:00:00Z")
	insertRecentsRow(t, inst, "row-gone", src+"missing-file-id", "2026-01-04T00:00:00Z")
	insertRecentsRow(t, inst, "row-ok1", src+fOK1.ID(), "2026-01-03T00:00:00Z")
	insertRecentsRow(t, inst, "row-ok2", src+fOK2.ID(), "2026-01-02T00:00:00Z")

	// The two invalid candidates do not consume page slots: the page is
	// refilled with the valid rows.
	body := getRecentsPage(t, e, token, "page[limit]=2")
	assert.Equal(t, []string{fOK1.ID(), fOK2.ID()}, recentsFileIDs(t, body))
	assertNoNext(t, body)
	body.Path("$.meta").Object().NotContainsKey("incomplete")

	// A trashed item is only hidden: it comes back on restore with the row.
	assert.NotNil(t, findRowByKey(t, inst, "row-trashed"), "trashed rows must be kept")
	// A destroyed item can never be shown again: its row is repaired away.
	assert.Nil(t, findRowByKey(t, inst, "row-gone"), "dead rows must be repaired")
}

func TestRecentsFullPageWithSkippedRows(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	f := createRecentsFile(t, inst, "full.txt", "text/plain")
	src := inst.Domain + "/"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Two dead rows on top, then 148 valid rows: a page of the maximum size
	// skips the first two and must still announce the remaining rows.
	insertRecentsRow(t, inst, "full-gone-1", src+"missing-1", base.Add(200*time.Minute).Format(time.RFC3339))
	insertRecentsRow(t, inst, "full-gone-2", src+"missing-2", base.Add(199*time.Minute).Format(time.RFC3339))
	for i := 0; i < 148; i++ {
		insertRecentsRow(t, inst, fmt.Sprintf("full-%03d", i), src+f.ID(),
			base.Add(time.Duration(148-i)*time.Minute).Format(time.RFC3339))
	}

	body := getRecentsPage(t, e, token, "page[limit]=100")
	assert.Len(t, recentsFileIDs(t, body), 100)
	body = followNext(t, e, token, body)
	assert.Len(t, recentsFileIDs(t, body), 48)
	assertNoNext(t, body)
}

func TestRecentsSharingAccess(t *testing.T) {
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

	owner := testutils.NewSetup(t, t.Name()+"-owner").GetTestInstance()
	user := testutils.NewSetup(t, t.Name()+"-user").GetTestInstance()
	testutils.WithFlag(t, user, "drive.recents.enabled", true)
	sess, err := session.New(user, session.LongRun, "")
	require.NoError(t, err)
	createRecentsApp(t, user, "drive")
	markRecentsSeeded(t, user)
	ts := recentsTestServer(t, user, sess)
	e := testutils.CreateTestClient(t, ts.URL)
	token := user.BuildAppToken("drive", sess.ID())

	shared := createRecentsFile(t, owner, "shared.txt", "text/plain")
	s := createRecentsSharing(t, owner, shared.ID(), user.Domain)
	require.NoError(t, recents.RecordActivity(user, owner, shared.ID(), recents.KindEdit))
	key := recents.ResourceKey(owner, shared.ID())

	t.Run("SharedItemIsVisible", func(t *testing.T) {
		body := getRecentsPage(t, e, token, "")
		assert.Equal(t, []string{shared.ID()}, recentsFileIDs(t, body))
		item := body.Path("$.data[0].attributes").Object()
		item.Value("source_instance").String().IsEqual(owner.Domain)
		// The owner's paths are never exposed.
		item.NotContainsKey("path")
	})

	var dateBefore string
	t.Run("RevokedAccessHidesIt", func(t *testing.T) {
		row := findRowByKey(t, user, key)
		require.NotNil(t, row)
		dateBefore = row.LastActivityAt

		s.Members[1].Status = sharing.MemberStatusRevoked
		require.NoError(t, couchdb.UpdateDoc(owner, s))

		body := getRecentsPage(t, e, token, "")
		assert.Empty(t, recentsFileIDs(t, body))
		assert.NotNil(t, findRowByKey(t, user, key), "losing access must not delete the row")
	})

	t.Run("RestoredAccessShowsItAgainWithoutFakeEdit", func(t *testing.T) {
		s.Members[1].Status = sharing.MemberStatusReady
		require.NoError(t, couchdb.UpdateDoc(owner, s))

		body := getRecentsPage(t, e, token, "")
		assert.Equal(t, []string{shared.ID()}, recentsFileIDs(t, body))

		row := findRowByKey(t, user, key)
		require.NotNil(t, row)
		assert.Equal(t, dateBefore, row.LastActivityAt, "no fake edit on the row")
	})
}

func TestRecentsIncomplete(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	fOK := createRecentsFile(t, inst, "ok.txt", "text/plain")
	insertRecentsRow(t, inst, "row-ok", inst.Domain+"/"+fOK.ID(), "2026-01-02T00:00:00Z")

	t.Run("MalformedRowIsRepaired", func(t *testing.T) {
		insertRecentsRow(t, inst, "row-bad", "no-slash-locator", "2026-01-03T00:00:00Z")

		body := getRecentsPage(t, e, token, "")
		assert.Equal(t, []string{fOK.ID()}, recentsFileIDs(t, body))
		body.Path("$.meta").Object().NotContainsKey("incomplete")
		assert.Nil(t, findRowByKey(t, inst, "row-bad"), "invalid rows must be repaired")
	})

	t.Run("UnavailableOwnerIsIncomplete", func(t *testing.T) {
		insertRecentsRow(t, inst, "row-ghost", "ghost.example.net/some-file", "2026-01-04T00:00:00Z")

		body := getRecentsPage(t, e, token, "")
		assert.Equal(t, []string{fOK.ID()}, recentsFileIDs(t, body))
		body.Path("$.meta.incomplete").Boolean().IsEqual(true)
		assert.NotNil(t, findRowByKey(t, inst, "row-ghost"), "an owner may come back: keep the row")
	})
}

func TestRecentsRouteShadowing(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	_, e, token := recentsSetup(t)

	// GET /files/recents must reach the recents handler, not the metadata
	// handler with file-id "recents" (which would 404).
	body := getRecentsPage(t, e, token, "")
	body.Path("$.data").Array().IsEmpty()
	body.Path("$.meta").Object().NotContainsKey("incomplete")
}

func TestRecentsMissingDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	// An instance created before the recents has no database until its first
	// recorded activity: the page is empty, not an error.
	require.NoError(t, couchdb.DeleteDB(inst, consts.FilesRecents))

	body := getRecentsPage(t, e, token, "")
	body.Path("$.data").Array().IsEmpty()
	assertNoNext(t, body)
}

func TestRecentsSeedOnFirstCall(t *testing.T) {
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
	sess, err := session.New(inst, session.LongRun, "")
	require.NoError(t, err)
	createRecentsApp(t, inst, "drive")
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	ts := recentsTestServer(t, inst, sess)
	e := testutils.CreateTestClient(t, ts.URL)
	token := inst.BuildAppToken("drive", sess.ID())

	// No recents rows and no seed marker: the first call seeds.
	f1 := createRecentsFile(t, inst, "seeded1.txt", "text/plain")
	f2 := createRecentsFile(t, inst, "seeded2.txt", "text/plain")

	body := getRecentsPage(t, e, token, "")
	ids := recentsFileIDs(t, body)
	require.GreaterOrEqual(t, len(ids), 2)
	// The seeded files first (most recently modified), then the seeded tree.
	assert.Equal(t, []string{f2.ID(), f1.ID()}, ids[:2])
	assert.NotEmpty(t, recentsRows(t, inst), "the first call must trigger the seed")
}

func TestRecentsOAuthPermissions(t *testing.T) {
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
	sess, err := session.New(inst, session.LongRun, "")
	require.NoError(t, err)
	testutils.WithFlag(t, inst, "drive.recents.enabled", true)
	ts := recentsTestServer(t, inst, sess)
	e := testutils.CreateTestClient(t, ts.URL)

	_, noFilesToken := setup.GetTestClient(consts.Settings)
	e.GET("/files/recents").
		WithHeader("Authorization", "Bearer "+noFilesToken).
		Expect().Status(403)

	_, filesToken := setup.GetTestClient(consts.Files)
	e.GET("/files/recents").
		WithHeader("Authorization", "Bearer "+filesToken).
		Expect().Status(200)
}

// createRecentsSharing creates an active additive sharing on the owner
// instance, rooted at rootID, with one ready recipient.
func createRecentsSharing(t *testing.T, owner *instance.Instance, rootID, memberDomain string) *sharing.Sharing {
	t.Helper()
	name, err := owner.SettingsPublicName()
	require.NoError(t, err)
	email, err := owner.SettingsEMail()
	require.NoError(t, err)
	now := time.Now()
	s := &sharing.Sharing{
		Active:        true,
		Owner:         true,
		Drive:         true,
		DriveRootType: sharing.DriveRootTypeFile,
		AppSlug:       "drive",
		AccessMode:    sharing.AccessModeAdditive,
		Members: []sharing.Member{
			{
				Status:   sharing.MemberStatusOwner,
				Name:     name,
				Email:    email,
				Instance: "https://" + owner.Domain,
			},
			{
				Status:   sharing.MemberStatusReady,
				Name:     memberDomain,
				Instance: "https://" + memberDomain,
			},
		},
		Rules: []sharing.Rule{
			{Title: "test", DocType: consts.Files, Values: []string{rootID}},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, couchdb.CreateDoc(owner, s))
	require.NoError(t, s.AddReferenceForSharing(owner, &s.Rules[0]))
	return s
}
