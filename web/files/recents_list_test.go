package files

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
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
	"github.com/cozy/cozy-stack/pkg/prefixer"
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

// insertOwnRow writes a crafted recents row of the instance on one of its
// files, keyed as in production.
func insertOwnRow(t *testing.T, inst *instance.Instance, file *vfs.FileDoc, date string) string {
	t.Helper()
	key := recents.ResourceKey(inst, file.ID())
	insertRecentsRow(t, inst, key, inst.Domain+"/"+file.ID(), date)
	return key
}

// insertDeadRow writes a crafted recents row of the instance on a file that
// does not exist.
func insertDeadRow(t *testing.T, inst *instance.Instance, fileID, date string) string {
	t.Helper()
	key := inst.DocID + ":" + fileID
	insertRecentsRow(t, inst, key, inst.Domain+"/"+fileID, date)
	return key
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

	insertOwnRow(t, inst, fD, "2026-01-04T00:00:00Z")
	insertOwnRow(t, inst, fE, "2026-01-04T00:00:00Z")
	insertOwnRow(t, inst, fA, "2026-01-03T00:00:00Z")
	insertOwnRow(t, inst, fB, "2026-01-02T00:00:00Z")
	insertOwnRow(t, inst, fC, "2026-01-01T00:00:00Z")

	body := getRecentsPage(t, e, token, "")

	// last_activity_at desc, then _id desc on ties
	tied := []string{fE.ID(), fD.ID()}
	if recents.ResourceKey(inst, tied[0]) < recents.ResourceKey(inst, tied[1]) {
		tied[0], tied[1] = tied[1], tied[0]
	}
	assert.Equal(t,
		[]string{tied[0], tied[1], fA.ID(), fB.ID(), fC.ID()},
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
		insertOwnRow(t, inst, files[i], dates[i])
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
	keyNew := insertOwnRow(t, inst, fNew, "2026-03-02T00:00:00Z")
	insertOwnRow(t, inst, fOld, "2026-03-01T00:00:00Z")

	body := getRecentsPage(t, e, token, "page[limit]=1")
	assert.Equal(t, []string{fNew.ID()}, recentsFileIDs(t, body))

	// A new activity moves the row of the cursor before the next page.
	row := findRowByKey(t, inst, keyNew)
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

	keyTrashed := insertOwnRow(t, inst, fTrashed, "2026-01-05T00:00:00Z")
	keyGone := insertDeadRow(t, inst, "missing-file-id", "2026-01-04T00:00:00Z")
	insertOwnRow(t, inst, fOK1, "2026-01-03T00:00:00Z")
	insertOwnRow(t, inst, fOK2, "2026-01-02T00:00:00Z")

	// The two invalid candidates do not consume page slots: the page is
	// refilled with the valid rows.
	body := getRecentsPage(t, e, token, "page[limit]=2")
	assert.Equal(t, []string{fOK1.ID(), fOK2.ID()}, recentsFileIDs(t, body))
	assertNoNext(t, body)
	body.Path("$.meta").Object().NotContainsKey("incomplete")

	// A trashed item is only hidden: it comes back on restore with the row.
	assert.NotNil(t, findRowByKey(t, inst, keyTrashed), "trashed rows must be kept")
	// A destroyed item can never be shown again: its row is repaired away.
	assert.Nil(t, findRowByKey(t, inst, keyGone), "dead rows must be repaired")
}

func TestRecentsFullPageWithSkippedRows(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Two dead rows on top, then 103 valid rows: a page of the maximum size
	// skips the first two and must still announce the remaining rows.
	insertDeadRow(t, inst, "missing-1", base.Add(200*time.Minute).Format(time.RFC3339))
	insertDeadRow(t, inst, "missing-2", base.Add(199*time.Minute).Format(time.RFC3339))
	for i := 0; i < 103; i++ {
		f := createRecentsFile(t, inst, fmt.Sprintf("full-%03d.txt", i), "text/plain")
		insertOwnRow(t, inst, f, base.Add(time.Duration(103-i)*time.Minute).Format(time.RFC3339))
	}

	body := getRecentsPage(t, e, token, "page[limit]=100")
	assert.Len(t, recentsFileIDs(t, body), 100)
	body = followNext(t, e, token, body)
	assert.Len(t, recentsFileIDs(t, body), 3)
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

	t.Run("UnreadableItemsAreIncomplete", func(t *testing.T) {
		// A failure of the owner's database is temporary: the row is kept.
		require.NoError(t, couchdb.DeleteDB(owner, consts.Files))

		body := getRecentsPage(t, e, token, "")
		assert.Empty(t, recentsFileIDs(t, body))
		body.Path("$.meta.incomplete").Boolean().IsTrue()
		assert.NotNil(t, findRowByKey(t, user, key), "an unreadable item must not delete the row")
	})
}

func TestRecentsIncomplete(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	fOK := createRecentsFile(t, inst, "ok.txt", "text/plain")
	insertOwnRow(t, inst, fOK, "2026-01-02T00:00:00Z")

	t.Run("MalformedRowIsRepaired", func(t *testing.T) {
		insertRecentsRow(t, inst, "row-bad", "no-slash-locator", "2026-01-03T00:00:00Z")

		body := getRecentsPage(t, e, token, "")
		assert.Equal(t, []string{fOK.ID()}, recentsFileIDs(t, body))
		body.Path("$.meta").Object().NotContainsKey("incomplete")
		assert.Nil(t, findRowByKey(t, inst, "row-bad"), "invalid rows must be repaired")
	})

	t.Run("GoneOwnerIsRepaired", func(t *testing.T) {
		key := "unknown-instance-id:some-file"
		insertRecentsRow(t, inst, key, "ghost.example.net/some-file", "2026-01-04T00:00:00Z")

		body := getRecentsPage(t, e, token, "")
		assert.Equal(t, []string{fOK.ID()}, recentsFileIDs(t, body))
		body.Path("$.meta").Object().NotContainsKey("incomplete")
		assert.Nil(t, findRowByKey(t, inst, key), "an owner with no successor never comes back")
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

// insertActivityRow writes a recents row with the dates of its activities.
func insertActivityRow(t *testing.T, inst, src *instance.Instance, fileID, edited, viewed string) string {
	t.Helper()
	row := &recents.FileDirActivity{
		DocID:          recents.ResourceKey(src, fileID),
		SourceLocator:  src.Domain + "/" + fileID,
		LastEditedAt:   edited,
		LastViewedAt:   viewed,
		LastActivityAt: max(edited, viewed),
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, row))
	return row.DocID
}

// recentsItemIDs returns the public IDs of the items of the page, in order.
func recentsItemIDs(t *testing.T, body *httpexpect.Value) []string {
	t.Helper()
	arr := body.Path("$.data").Array()
	n := int(arr.Length().Raw())
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, arr.Value(i).Object().Value("id").String().Raw())
	}
	return ids
}

func TestRecentsMixedPage(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	user, e, token := recentsSetup(t)
	owner1 := testutils.NewSetup(t, t.Name()+"-owner1").GetTestInstance()
	owner2 := testutils.NewSetup(t, t.Name()+"-owner2").GetTestInstance()

	own1 := createRecentsFile(t, user, "own1.txt", "text/plain")
	own2 := createRecentsFile(t, user, "own2.txt", "text/plain")
	granted := createRecentsFile(t, owner1, "granted.txt", "text/plain")
	createRecentsSharing(t, owner1, granted.ID(), user.Domain)
	revoked := createRecentsFile(t, owner1, "revoked.txt", "text/plain")
	s := createRecentsSharing(t, owner1, revoked.ID(), user.Domain)
	s.Members[1].Status = sharing.MemberStatusRevoked
	require.NoError(t, couchdb.UpdateDoc(owner1, s))
	readOnly := createRecentsFile(t, owner2, "read-only.txt", "text/plain")
	s = createRecentsSharing(t, owner2, readOnly.ID(), user.Domain)
	s.Members[1].ReadOnly = true
	require.NoError(t, couchdb.UpdateDoc(owner2, s))

	insertOwnRow(t, user, own1, "2026-01-06T00:00:00Z")
	insertRecentsRow(t, user, recents.ResourceKey(owner1, granted.ID()),
		owner1.Domain+"/"+granted.ID(), "2026-01-05T00:00:00Z")
	revokedKey := recents.ResourceKey(owner1, revoked.ID())
	insertRecentsRow(t, user, revokedKey,
		owner1.Domain+"/"+revoked.ID(), "2026-01-04T00:00:00Z")
	insertRecentsRow(t, user, recents.ResourceKey(owner2, readOnly.ID()),
		owner2.Domain+"/"+readOnly.ID(), "2026-01-03T00:00:00Z")
	insertOwnRow(t, user, own2, "2026-01-02T00:00:00Z")

	expected := []string{own1.ID(), granted.ID(), readOnly.ID(), own2.ID()}
	body := getRecentsPage(t, e, token, "")
	assert.Equal(t, expected, recentsFileIDs(t, body))
	body.Path("$.meta").Object().NotContainsKey("incomplete")
	body.Path("$.data[0].attributes").Object().NotContainsKey("source_instance")
	body.Path("$.data[1].attributes.source_instance").String().IsEqual(owner1.Domain)
	body.Path("$.data[2].attributes.source_instance").String().IsEqual(owner2.Domain)
	assert.NotNil(t, findRowByKey(t, user, revokedKey), "losing access must not delete the row")

	body = getRecentsPage(t, e, token, "page[limit]=2")
	seen := recentsFileIDs(t, body)
	body = followNext(t, e, token, body)
	seen = append(seen, recentsFileIDs(t, body)...)
	assert.Equal(t, expected, seen)
	assertNoNext(t, body)
}

func TestRecentsGroupedRepairs(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	fOK1 := createRecentsFile(t, inst, "ok1.txt", "text/plain")
	fOK2 := createRecentsFile(t, inst, "ok2.txt", "text/plain")
	dead1 := insertDeadRow(t, inst, "dead-1", "2026-01-06T00:00:00Z")
	dead2 := insertDeadRow(t, inst, "dead-2", "2026-01-05T00:00:00Z")
	insertOwnRow(t, inst, fOK1, "2026-01-04T00:00:00Z")
	dead3 := insertDeadRow(t, inst, "dead-3", "2026-01-03T00:00:00Z")
	insertOwnRow(t, inst, fOK2, "2026-01-02T00:00:00Z")
	dead4 := insertDeadRow(t, inst, "dead-4", "2026-01-01T00:00:00Z")

	body := getRecentsPage(t, e, token, "page[limit]=1")
	assert.Equal(t, []string{fOK1.ID()}, recentsFileIDs(t, body))
	body.Path("$.meta").Object().NotContainsKey("incomplete")

	// The examined rows are repaired, up to the proof that more rows follow.
	assert.Nil(t, findRowByKey(t, inst, dead1))
	assert.Nil(t, findRowByKey(t, inst, dead2))
	assert.Nil(t, findRowByKey(t, inst, dead3))
	assert.NotNil(t, findRowByKey(t, inst, dead4), "a row beyond the page is not examined")

	body = followNext(t, e, token, body)
	assert.Equal(t, []string{fOK2.ID()}, recentsFileIDs(t, body))
	body.Path("$.meta").Object().NotContainsKey("incomplete")
	assertNoNext(t, body)
	assert.Nil(t, findRowByKey(t, inst, dead4))
}

func TestRecentsDeadRowsStopConsumingTheBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fOK1 := createRecentsFile(t, inst, "ok1.txt", "text/plain")
	fOK2 := createRecentsFile(t, inst, "ok2.txt", "text/plain")
	insertOwnRow(t, inst, fOK1, base.Add(time.Minute).Format(time.RFC3339))
	insertOwnRow(t, inst, fOK2, base.Format(time.RFC3339))
	for i := 0; i < 201; i++ {
		insertDeadRow(t, inst, fmt.Sprintf("dead-%03d", i),
			base.Add(time.Duration(300-i)*time.Minute).Format(time.RFC3339))
	}

	// The work limit is reached on dead rows only.
	body := getRecentsPage(t, e, token, "")
	assert.Empty(t, recentsFileIDs(t, body))
	body.Path("$.meta.incomplete").Boolean().IsTrue()
	// All the examined rows are deleted: the next page starts again.
	body.Path("$.links.next").String().NotContains("cursor")

	body = followNext(t, e, token, body)
	assert.Equal(t, []string{fOK1.ID(), fOK2.ID()}, recentsFileIDs(t, body))
	body.Path("$.meta").Object().NotContainsKey("incomplete")
	assertNoNext(t, body)
}

func TestRecentsPageOfOneWithHiddenRows(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	for i := 0; i < 10; i++ {
		f := createRecentsFile(t, inst, fmt.Sprintf("trashed-%d.txt", i), "text/plain")
		e.DELETE("/files/"+f.ID()).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)
		insertOwnRow(t, inst, f, fmt.Sprintf("2026-01-%02dT00:00:00Z", 20-i))
	}
	fOK := createRecentsFile(t, inst, "ok.txt", "text/plain")
	insertOwnRow(t, inst, fOK, "2026-01-01T00:00:00Z")

	body := getRecentsPage(t, e, token, "page[limit]=1")
	assert.Equal(t, []string{fOK.ID()}, recentsFileIDs(t, body))
	body.Path("$.meta").Object().NotContainsKey("incomplete")
	assertNoNext(t, body)
	assert.Len(t, recentsRows(t, inst), 11, "hidden rows are kept")
}

func TestRecentsPaginationAcrossIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	files := make([]*vfs.FileDoc, 4)
	for i := range files {
		files[i] = createRecentsFile(t, inst, fmt.Sprintf("ids-%d.txt", i), "text/plain")
	}
	sort.Slice(files, func(i, j int) bool {
		return recents.ResourceKey(inst, files[i].ID()) < recents.ResourceKey(inst, files[j].ID())
	})
	// On the date of a cursor, the older rows can have a greater _id: the
	// position must only bound the traversal by its date.
	insertOwnRow(t, inst, files[1], "2026-01-03T00:00:00Z")
	insertOwnRow(t, inst, files[2], "2026-01-02T00:00:00Z")
	insertOwnRow(t, inst, files[0], "2026-01-02T00:00:00Z")
	insertOwnRow(t, inst, files[3], "2026-01-01T00:00:00Z")
	expected := []string{files[1].ID(), files[2].ID(), files[0].ID(), files[3].ID()}

	var seen []string
	body := getRecentsPage(t, e, token, "page[limit]=1")
	for {
		seen = append(seen, recentsFileIDs(t, body)...)
		body.Path("$.meta").Object().NotContainsKey("incomplete")
		if len(seen) >= len(expected) {
			break
		}
		body = followNext(t, e, token, body)
	}
	assert.Equal(t, expected, seen)
	assertNoNext(t, body)
}

func TestRecentsStaleLocatorIsRefreshed(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	inst, e, token := recentsSetup(t)

	// The locator carries an alias of the domain: the key tells the source,
	// and the row stays a row of the user's own files.
	f := createRecentsFile(t, inst, "alias.txt", "text/plain")
	key := recents.ResourceKey(inst, f.ID())
	row := &recents.FileDirActivity{
		DocID:          key,
		SourceLocator:  "alias.example.net/" + f.ID(),
		LastEditedAt:   "2026-01-02T00:00:00Z",
		LastActivityAt: "2026-01-02T00:00:00Z",
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, row))

	body := getRecentsPage(t, e, token, "")
	assert.Equal(t, []string{f.ID()}, recentsFileIDs(t, body))
	body.Path("$.data[0].attributes").Object().NotContainsKey("source_instance")

	stored := findRowByKey(t, inst, key)
	require.NotNil(t, stored)
	assert.Equal(t, inst.Domain+"/"+f.ID(), stored.SourceLocator)
	assert.Equal(t, "2026-01-02T00:00:00Z", stored.LastEditedAt, "the dates are kept")
}

// createRecentsFileWithID creates a file with a given ID, as the import of a
// move does.
func createRecentsFileWithID(t *testing.T, inst *instance.Instance, id, name string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, consts.RootDirID, -1, nil,
		"text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	doc.SetID(id)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return doc
}

// markRecentsMoved marks an instance as moved to another one, as the end of a
// move does.
func markRecentsMoved(t *testing.T, from, to *instance.Instance) {
	t.Helper()
	doc, err := from.SettingsDocument()
	require.NoError(t, err)
	doc.M["moved_to"] = "https://" + to.Domain
	require.NoError(t, couchdb.UpdateDoc(from, doc))
	var stored instance.Instance
	require.NoError(t, couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Instances, from.DocID, &stored))
	stored.Moved = true
	require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, &stored))
}

func TestRecentsMovedSource(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	user, e, token := recentsSetup(t)
	old := testutils.NewSetup(t, t.Name()+"-old").GetTestInstance()
	moved := testutils.NewSetup(t, t.Name()+"-new").GetTestInstance()

	// The move keeps the file IDs.
	f := createRecentsFile(t, old, "migrated.txt", "text/plain")
	createRecentsFileWithID(t, moved, f.ID(), "migrated.txt")
	createRecentsSharing(t, moved, f.ID(), user.Domain)
	g := createRecentsFile(t, old, "merged.txt", "text/plain")
	createRecentsFileWithID(t, moved, g.ID(), "merged.txt")
	createRecentsSharing(t, moved, g.ID(), user.Domain)

	oldF := insertActivityRow(t, user, old, f.ID(), "2026-01-02T00:00:00Z", "")
	oldG := insertActivityRow(t, user, old, g.ID(), "2026-01-01T00:00:00Z", "")
	// A new activity on the moved file, after the move.
	newG := insertActivityRow(t, user, moved, g.ID(), "", "2026-01-03T00:00:00Z")
	newF := recents.ResourceKey(moved, f.ID())
	markRecentsMoved(t, old, moved)

	// The row of g under the new instance comes first: the old one is only
	// merged into it.
	expected := []string{recents.PublicID(newG), recents.PublicID(newF)}
	body := getRecentsPage(t, e, token, "")
	assert.Equal(t, []string{g.ID(), f.ID()}, recentsFileIDs(t, body))
	assert.Equal(t, expected, recentsItemIDs(t, body))
	body.Path("$.data[0].attributes.source_instance").String().IsEqual(moved.Domain)
	body.Path("$.data[1].attributes.source_instance").String().IsEqual(moved.Domain)
	body.Path("$.meta").Object().NotContainsKey("incomplete")

	assert.Nil(t, findRowByKey(t, user, oldF), "the old row is migrated")
	assert.Nil(t, findRowByKey(t, user, oldG), "the old row is merged")
	rowF := findRowByKey(t, user, newF)
	require.NotNil(t, rowF)
	assert.Equal(t, moved.Domain+"/"+f.ID(), rowF.SourceLocator)
	assert.Equal(t, "2026-01-02T00:00:00Z", rowF.LastEditedAt)
	assert.Equal(t, "2026-01-02T00:00:00Z", rowF.LastActivityAt)
	rowG := findRowByKey(t, user, newG)
	require.NotNil(t, rowG)
	assert.Equal(t, "2026-01-01T00:00:00Z", rowG.LastEditedAt)
	assert.Equal(t, "2026-01-03T00:00:00Z", rowG.LastViewedAt)
	assert.Equal(t, "2026-01-03T00:00:00Z", rowG.LastActivityAt)

	body = getRecentsPage(t, e, token, "")
	assert.Equal(t, expected, recentsItemIDs(t, body))
	body.Path("$.meta").Object().NotContainsKey("incomplete")
}

func TestRecentsMovedSourceAtPageEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	user, e, token := recentsSetup(t)
	old := testutils.NewSetup(t, t.Name()+"-old").GetTestInstance()
	moved := testutils.NewSetup(t, t.Name()+"-new").GetTestInstance()

	files := make([]*vfs.FileDoc, 2)
	for i := range files {
		files[i] = createRecentsFile(t, old, fmt.Sprintf("moved-%d.txt", i), "text/plain")
		createRecentsFileWithID(t, moved, files[i].ID(), files[i].DocName)
		createRecentsSharing(t, moved, files[i].ID(), user.Domain)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ID() > files[j].ID() })
	top := createRecentsFile(t, user, "top.txt", "text/plain")
	bottom := createRecentsFile(t, user, "bottom.txt", "text/plain")

	insertOwnRow(t, user, top, "2026-01-03T00:00:00Z")
	// Two rows of the same date: the first one ends the first page, and its
	// migration deletes it.
	insertActivityRow(t, user, old, files[0].ID(), "2026-01-02T00:00:00Z", "")
	insertActivityRow(t, user, old, files[1].ID(), "2026-01-02T00:00:00Z", "")
	insertOwnRow(t, user, bottom, "2026-01-01T00:00:00Z")
	markRecentsMoved(t, old, moved)

	body := getRecentsPage(t, e, token, "page[limit]=2")
	assert.Equal(t, []string{top.ID(), files[0].ID()}, recentsFileIDs(t, body))
	seen := recentsItemIDs(t, body)
	for i := 0; i < 5; i++ {
		if _, ok := body.Object().Raw()["links"]; !ok {
			break
		}
		body = followNext(t, e, token, body)
		body.Path("$.meta").Object().NotContainsKey("incomplete")
		seen = append(seen, recentsItemIDs(t, body)...)
	}
	assertNoNext(t, body)

	// The cursor stays on the last row that is still there: the migrated item
	// ending the first page comes back under the same public ID, and the
	// other row of its date is not skipped.
	assert.Equal(t, []string{
		recents.PublicID(recents.ResourceKey(user, top.ID())),
		recents.PublicID(recents.ResourceKey(moved, files[0].ID())),
		recents.PublicID(recents.ResourceKey(moved, files[0].ID())),
		recents.PublicID(recents.ResourceKey(moved, files[1].ID())),
		recents.PublicID(recents.ResourceKey(user, bottom.ID())),
	}, seen)
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
