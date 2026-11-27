package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/opensearch"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"
)

func TestBuildQuery(t *testing.T) {
	body := buildQuery("bob.example.net", "report", []string{"alice.example.net:d1"}, nil, 20, nil)
	data, err := json.Marshal(body)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"size": 20,
		"query": {"bool": {
			"must": [{"bool": {"should": [
				{"match": {"name": {"query": "report", "minimum_should_match": "75%"}}},
				{"multi_match": {"query": "report", "type": "bool_prefix",
					"fields": ["name.prefix", "name.prefix._2gram", "name.prefix._3gram"]}},
				{"match": {"name.exact": {"query": "report", "boost": 4}}}
			], "minimum_should_match": 1}}],
			"filter": [{"bool": {"should": [
				{"term": {"owner_instance_id": "bob.example.net"}},
				{"terms": {"scoped_dir_key": ["alice.example.net:d1"]}}
			], "minimum_should_match": 1}}],
			"must_not": [{"term": {"trashed": true}}]
		}},
		"sort": [{"_score": "desc"}, {"resource_key": "asc"}]
	}`, string(data))

	t.Run("folder keys are sent in chunks", func(t *testing.T) {
		termsChunkSize = 2
		t.Cleanup(func() { termsChunkSize = 65536 })
		body := buildQuery("bob.example.net", "x", []string{"k1", "k2", "k3"}, nil, 20, nil)
		filter := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
		visible := filter[0].(map[string]any)["bool"].(map[string]any)["should"].([]any)
		require.Len(t, visible, 3)
		assert.Equal(t, []string{"k1", "k2"}, visible[1].(map[string]any)["terms"].(map[string]any)["scoped_dir_key"])
		assert.Equal(t, []string{"k3"}, visible[2].(map[string]any)["terms"].(map[string]any)["scoped_dir_key"])
	})

	t.Run("files of single-file drives", func(t *testing.T) {
		body := buildQuery("bob.example.net", "x", nil, []string{"alice.example.net:f1"}, 20, nil)
		filter := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
		visible := filter[0].(map[string]any)["bool"].(map[string]any)["should"].([]any)
		require.Len(t, visible, 2)
		assert.Equal(t, map[string]any{"terms": map[string]any{"resource_key": []string{"alice.example.net:f1"}}}, visible[1])
	})

	t.Run("only own files without drives", func(t *testing.T) {
		body := buildQuery("bob.example.net", "x", nil, nil, 20, nil)
		filter := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
		assert.Len(t, filter[0].(map[string]any)["bool"].(map[string]any)["should"], 1)
	})

	t.Run("next page", func(t *testing.T) {
		body := buildQuery("bob.example.net", "x", nil, nil, 20, []any{1.5, "bob.example.net:f1"})
		assert.Equal(t, []any{1.5, "bob.example.net:f1"}, body["search_after"])
	})
}

func TestCursor(t *testing.T) {
	binding := cursorBinding("bob.example.net", "report")
	cursor, err := encodeCursor([]any{2.25, "alice.example.net:f1"}, binding)
	require.NoError(t, err)
	after, err := decodeCursor(cursor, binding)
	require.NoError(t, err)
	assert.Equal(t, []any{2.25, "alice.example.net:f1"}, after)

	after, err = decodeCursor("", binding)
	require.NoError(t, err)
	assert.Nil(t, after)

	for _, other := range []string{cursorBinding("bob.example.net", "budget"), cursorBinding("carol.example.net", "report")} {
		_, err := decodeCursor(cursor, other)
		assert.ErrorIs(t, err, ErrInvalidCursor, "a cursor is for the same caller and query")
	}
	for _, bad := range []string{"%%%", "bm90IGpzb24", "WzEsMiwzXQ", "WyJhIiwiYiJd"} {
		_, err := decodeCursor(bad, binding)
		assert.ErrorIs(t, err, ErrInvalidCursor, bad)
	}
}

func TestCompareScopes(t *testing.T) {
	outer := ScopeDocument{SharingID: "s1", SubtreeDirKeys: []string{"a:root", "a:sub"}}
	inner := ScopeDocument{SharingID: "s0", SubtreeDirKeys: []string{"a:sub"}}
	assert.Negative(t, compareScopes("bob.example.net", outer, inner), "the outermost drive first")
	outer.ReadOnlyPrincipals = []string{"bob.example.net"}
	assert.Positive(t, compareScopes("bob.example.net", outer, inner), "a drive where bob can write first")
	assert.Negative(t, compareScopes("carol.example.net", outer, inner), "only bob is read-only")
}

func TestMemberScopes(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		path, body = r.URL.Path, string(data)
		_, _ = fmt.Fprintf(w, `{"docs":[
			{"_id":"s0","found":true,"_source":{"sharing_id":"s0","owner_instance_id":"alice.example.net","root_dir_id":"sub1",
				"active_member_principals":["%[1]s"],"subtree_dir_keys":["alice.example.net:sub1"]}},
			{"_id":"s1","found":true,"_source":{"sharing_id":"s1","owner_instance_id":"alice.example.net","root_dir_id":"root1",
				"active_member_principals":["dave.example.net","%[1]s"],
				"subtree_dir_keys":["alice.example.net:root1","alice.example.net:sub1"]}},
			{"_id":"s2","found":true,"_source":{"sharing_id":"s2","owner_instance_id":"carol.example.net","root_dir_id":"root2",
				"active_member_principals":["dave.example.net"],"subtree_dir_keys":["carol.example.net:root2"]}},
			{"_id":"s3","found":true,"_source":{"sharing_id":"s3","owner_instance_id":"alice.example.net","root_dir_id":"",
				"root_file_id":"file3","active_member_principals":["%[1]s"],"subtree_dir_keys":null}},
			{"_id":"s4","found":false}]}`, inst.Domain)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := opensearch.NewClient(opensearch.Options{URL: u})
	ctx := context.Background()

	t.Run("no drive", func(t *testing.T) {
		dirKeys, fileKeys, drives, err := memberScopes(ctx, client, inst)
		require.NoError(t, err)
		assert.Empty(t, dirKeys)
		assert.Empty(t, fileKeys)
		assert.Empty(t, drives)
		assert.Empty(t, path, "no request to OpenSearch")
	})

	t.Run("drives held and listing the caller", func(t *testing.T) {
		// Every sharing of the caller is looked up: only an active drive has a
		// scope doc, and it lists the caller only if the caller accepted it
		for _, s := range []*sharing.Sharing{
			{SID: "s0", Drive: true, Active: true},
			{SID: "s1", Drive: true, Active: true},
			{SID: "s2", Drive: true, Active: true},
			{SID: "s3", Drive: true, Active: true},
			{SID: "s4", Drive: true, Active: true},
			{SID: "s5", Active: true},
		} {
			require.NoError(t, couchdb.CreateNamedDoc(inst, s))
		}
		dirKeys, fileKeys, drives, err := memberScopes(ctx, client, inst)
		require.NoError(t, err)
		assert.Equal(t, "/share_scopes_v1/_mget", path)
		var sent struct{ IDs []string }
		require.NoError(t, json.Unmarshal([]byte(body), &sent))
		assert.ElementsMatch(t, []string{"s0", "s1", "s2", "s3", "s4", "s5"}, sent.IDs)
		// s2 does not list the caller, s4 and s5 have no scope doc
		assert.Equal(t, map[string]string{
			"alice.example.net:root1": "s1",
			"alice.example.net:sub1":  "s1", // in the outermost drive
		}, dirKeys)
		assert.Equal(t, map[string]string{"alice.example.net:file3": "s3"}, fileKeys)
		assert.Equal(t, map[string]Drive{
			"s0": {OwnerInstanceID: "alice.example.net", RootDirID: "sub1"},
			"s1": {OwnerInstanceID: "alice.example.net", RootDirID: "root1"},
			"s3": {OwnerInstanceID: "alice.example.net", RootFileID: "file3"},
		}, drives)
	})
}

func TestSearchWithOpenSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	osURL := os.Getenv("COZY_TEST_OPENSEARCH_URL")
	if osURL == "" {
		t.Skip("COZY_TEST_OPENSEARCH_URL is not set")
	}
	t.Setenv("COZY_SEARCH_OPENSEARCH_URL", osURL)
	t.Setenv("COZY_SEARCH_OPENSEARCH_INDEX_PREFIX", fmt.Sprintf("test-%d", time.Now().UnixNano()))
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	owner := testutils.NewSetup(t, t.Name()+"_owner").GetTestInstance()
	caller := testutils.NewSetup(t, t.Name()+"_caller").GetTestInstance()
	client := config.GetConfig().Search
	ctx := context.Background()
	require.NoError(t, EnsureIndices(ctx, client))

	// Files of the caller
	own := createTextFile(t, caller, consts.RootDirID, "budget report.txt")
	createTextFile(t, caller, consts.RootDirID, "invoice invoice")
	createTextFile(t, caller, consts.RootDirID, "invoice")
	trashed := createTextFile(t, caller, consts.RootDirID, "old report.txt")
	_, err := vfs.TrashFile(caller.VFS(), trashed)
	require.NoError(t, err)

	// A drive of the owner where the caller is a member, and one where not
	member := sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://" + caller.Domain}
	// Read-only: its scope doc has read_only_principals, which the mapping accepts
	stranger := sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://stranger.example.net", ReadOnly: true}
	shared := createDrive(t, owner, caller, "drive-member", "/Team", member, "Sub")
	inDrive := createTextFile(t, owner, shared.ID(), "team report.txt")
	sub, err := owner.VFS().DirByPath("/Team/Sub")
	require.NoError(t, err)
	inSub := createTextFile(t, owner, sub.ID(), "agenda.txt")
	other := createDrive(t, owner, caller, "drive-stranger", "/Other", stranger)
	createTextFile(t, owner, other.ID(), "secret report.txt")
	// The owner lists the caller, but the caller does not hold the drive: a
	// member took the address of the caller
	claimed := createDrive(t, owner, caller, "drive-claimed", "/Claimed", member)
	createTextFile(t, owner, claimed.ID(), "claimed report.txt")
	claimedCopy, err := sharing.FindSharing(caller, "drive-claimed")
	require.NoError(t, err)
	require.NoError(t, couchdb.DeleteDoc(caller, claimedCopy))
	createTextFile(t, owner, consts.RootDirID, "private report.txt")

	log := logger.WithNamespace("search-test")
	require.NoError(t, Index(ctx, owner, log))
	require.NoError(t, Index(ctx, caller, log))

	names := func(res *Results) map[string]SearchHit {
		out := make(map[string]SearchHit)
		for _, h := range res.Hits {
			out[h.Name] = h
		}
		return out
	}
	var res *Results
	require.Eventually(t, func() bool {
		res, err = Search(ctx, client, caller, "report", 0, "")
		return err == nil && len(res.Hits) == 2
	}, 5*time.Second, 200*time.Millisecond)
	found := names(res)
	require.Contains(t, found, "budget report.txt")
	require.Contains(t, found, "team report.txt")
	assert.Equal(t, SearchHit{
		Hit:  Hit{OwnerInstanceID: caller.Domain, DocID: own.ID(), DirID: consts.RootDirID},
		Type: "file", Name: "budget report.txt", Mime: "text/plain", Class: "text",
		UpdatedAt: found["budget report.txt"].UpdatedAt,
	}, found["budget report.txt"])
	assert.Equal(t, "drive-member", found["team report.txt"].DriveID)
	assert.Equal(t, inDrive.ID(), found["team report.txt"].DocID)
	assert.Empty(t, res.Cursor, "a single page")
	assert.Contains(t, res.Drives, "drive-member")
	assert.NotContains(t, res.Drives, "drive-stranger")

	t.Run("pages", func(t *testing.T) {
		first, err := Search(ctx, client, caller, "report", 1, "")
		require.NoError(t, err)
		require.Len(t, first.Hits, 1)
		require.NotEmpty(t, first.Cursor)
		second, err := Search(ctx, client, caller, "report", 1, first.Cursor)
		require.NoError(t, err)
		require.Len(t, second.Hits, 1)
		assert.NotEqual(t, first.Hits[0].DocID, second.Hits[0].DocID)
		assert.Empty(t, second.Cursor, "the last page, even if full")
	})

	t.Run("a file in a subfolder of a drive", func(t *testing.T) {
		res, err := Search(ctx, client, caller, "agenda", 0, "")
		require.NoError(t, err)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, inSub.ID(), res.Hits[0].DocID)
		assert.Equal(t, "drive-member", res.Hits[0].DriveID)
	})

	t.Run("prefix of a word being typed", func(t *testing.T) {
		res, err := Search(ctx, client, caller, "budg", 0, "")
		require.NoError(t, err)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, "budget report.txt", res.Hits[0].Name)

		// Two letters give no n-gram: only the prefix clause matches them
		res, err = Search(ctx, client, caller, "bu", 0, "")
		require.NoError(t, err)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, "budget report.txt", res.Hits[0].Name)
	})

	t.Run("substring inside a word", func(t *testing.T) {
		// Not a prefix: only the n-gram clause matches it
		res, err := Search(ctx, client, caller, "udge", 0, "")
		require.NoError(t, err)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, "budget report.txt", res.Hits[0].Name)
	})

	t.Run("the whole name comes first", func(t *testing.T) {
		// Without the clause on the whole name, the name with the word twice
		// would come first
		res, err := Search(ctx, client, caller, "invoice", 0, "")
		require.NoError(t, err)
		require.Len(t, res.Hits, 2)
		assert.Equal(t, "invoice", res.Hits[0].Name)
	})

	t.Run("accents and other scripts", func(t *testing.T) {
		// Names stored decomposed (NFD), as macOS often writes them
		chaine := norm.NFD.String("Chaîne de valeur.txt")
		journal := norm.NFD.String("Ημερολόγιο ταξιδιού.txt")
		newFile := norm.NFD.String("Новый файл.txt")
		minutes := norm.NFD.String("회의록.txt")
		saida := norm.NFD.String("Saïda.txt")
		for _, name := range []string{
			"Отчёт за квартал.pdf", "Straße Plan.txt", "Ελληνικές σημειώσεις.txt",
			"会议记录.docx", "تقرير سنوي.pdf", "Çalışma raporu.txt", "naïve café.txt",
			"Ærøskøbing kort.png", chaine, journal, newFile, minutes, saida,
		} {
			createTextFile(t, caller, consts.RootDirID, name)
		}
		require.NoError(t, Index(ctx, caller, log))
		for q, expected := range map[string]string{
			"отчет":       "Отчёт за квартал.pdf",
			"квартал":     "Отчёт за квартал.pdf",
			"strasse":     "Straße Plan.txt",
			"straße":      "Straße Plan.txt",
			"σημειωσεις":  "Ελληνικές σημειώσεις.txt",
			"会议":          "会议记录.docx",
			"记录":          "会议记录.docx",
			"تقرير":       "تقرير سنوي.pdf",
			"calisma":     "Çalışma raporu.txt",
			"cafe":        "naïve café.txt",
			"aeroskobing": "Ærøskøbing kort.png",
			"ae":          "Ærøskøbing kort.png",
			"chaine":      chaine,
			"chaîne":      chaine,
			"ημερολογιο":  journal,
			"файл":        newFile,
			"회의록":         minutes,
			"saida":       saida,
			// A query pasted decomposed: Hangul jamo never match the syllables
			norm.NFD.String("회의록"): minutes,
		} {
			var res *Results
			require.Eventually(t, func() bool {
				res, err = Search(ctx, client, caller, q, 0, "")
				return err == nil && len(res.Hits) > 0
			}, 5*time.Second, 200*time.Millisecond, q)
			require.Len(t, res.Hits, 1, q)
			assert.Equal(t, expected, res.Hits[0].Name, q)
		}
	})

	t.Run("single-file drive", func(t *testing.T) {
		private, err := vfs.Mkdir(owner.VFS(), "/Private", nil)
		require.NoError(t, err)
		file := createTextFile(t, owner, private.ID(), "minutes.txt")
		createTextFile(t, owner, private.ID(), "minutes draft.txt") // not shared
		members := []sharing.Member{{Status: sharing.MemberStatusOwner, Instance: "https://" + owner.Domain}, member}
		rules := []sharing.Rule{{DocType: consts.Files, Values: []string{file.ID()}}}
		s := &sharing.Sharing{SID: "drive-file", Drive: true, Active: true, Owner: true,
			DriveRootType: sharing.DriveRootTypeFile, Rules: rules, Members: members}
		require.NoError(t, couchdb.CreateNamedDoc(owner, s))
		recipientSide := &sharing.Sharing{SID: "drive-file", Drive: true, Active: true,
			DriveRootType: sharing.DriveRootTypeFile, Rules: rules, Members: members}
		require.NoError(t, couchdb.CreateNamedDoc(caller, recipientSide))
		require.NoError(t, Index(ctx, owner, log)) // also writes the scope doc

		var res *Results
		require.Eventually(t, func() bool {
			res, err = Search(ctx, client, caller, "minutes", 0, "")
			return err == nil && len(res.Hits) > 0
		}, 5*time.Second, 200*time.Millisecond)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, file.ID(), res.Hits[0].DocID)
		assert.Equal(t, "drive-file", res.Hits[0].DriveID)
		assert.Equal(t, Drive{OwnerInstanceID: owner.Domain, RootFileID: file.ID()}, res.Drives["drive-file"])
	})

	t.Run("invalid cursor", func(t *testing.T) {
		_, err := Search(ctx, client, caller, "report", 0, "nope")
		assert.ErrorIs(t, err, ErrInvalidCursor)
		first, err := Search(ctx, client, caller, "report", 1, "")
		require.NoError(t, err)
		_, err = Search(ctx, client, caller, "budget", 1, first.Cursor)
		assert.ErrorIs(t, err, ErrInvalidCursor, "a cursor is for its own query")
		_, err = Search(ctx, client, owner, "report", 1, first.Cursor)
		assert.ErrorIs(t, err, ErrInvalidCursor, "and its own caller")
	})
}

// createDrive creates a folder shared as a drive by owner, with the given
// subfolders: the sharing on both sides, and the scope doc.
func createDrive(t *testing.T, owner, recipient *instance.Instance, id, path string, member sharing.Member, subfolders ...string) *vfs.DirDoc {
	t.Helper()
	root, err := vfs.Mkdir(owner.VFS(), path, nil)
	require.NoError(t, err)
	for _, name := range subfolders {
		_, err := vfs.Mkdir(owner.VFS(), path+"/"+name, nil)
		require.NoError(t, err)
	}
	members := []sharing.Member{{Status: sharing.MemberStatusOwner, Instance: "https://" + owner.Domain}, member}
	rules := []sharing.Rule{{DocType: consts.Files, Values: []string{root.ID()}}}
	ownerSide := &sharing.Sharing{SID: id, Drive: true, Active: true, Owner: true, Description: path[1:], Rules: rules, Members: members}
	require.NoError(t, couchdb.CreateNamedDoc(owner, ownerSide))
	recipientSide := &sharing.Sharing{SID: id, Drive: true, Active: true, Description: path[1:], Rules: rules, Members: members}
	require.NoError(t, couchdb.CreateNamedDoc(recipient, recipientSide))
	doc, keep, err := NewScopeDocument(owner, ownerSide)
	require.NoError(t, err)
	require.True(t, keep)
	require.NoError(t, UpsertScope(context.Background(), config.GetConfig().Search, doc))
	return root
}
