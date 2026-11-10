package ai

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestIndexFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{})

	ts := setup.GetTestServer("/ai", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	fs := inst.VFS()
	kb, err := vfs.MkdirAll(fs, "/KB")
	require.NoError(t, err)
	_, err = vfs.MkdirAll(fs, "/Other")
	require.NoError(t, err)
	inKB := writeIndexTestFile(t, fs, "/KB/a.txt", "alpha")
	outside := writeIndexTestFile(t, fs, "/Other/b.txt", "beta")
	assistant := couchdb.JSONDoc{Type: consts.ChatAssistants, M: map[string]interface{}{
		"relationships": map[string]interface{}{"knowledgeBase": map[string]interface{}{
			"data": []map[string]interface{}{{"_id": kb.DocID, "_type": consts.Files}},
		}},
	}}
	require.NoError(t, couchdb.CreateDoc(inst, &assistant))

	get := func(t *testing.T, path, fileID string) *httpexpect.Response {
		t.Helper()
		token, err := rag.MakeIndexToken(inst, fileID)
		require.NoError(t, err)
		e := testutils.CreateTestClient(t, ts.URL)
		return e.GET(path).WithHeader("Authorization", "Bearer "+token).Expect()
	}

	t.Run("the state of a file in a knowledge base", func(t *testing.T) {
		obj := get(t, rag.IndexFilesPath+inKB.DocID, inKB.DocID).Status(200).JSON().Object()
		obj.Value("name").String().IsEqual("a.txt")
		obj.Value("workspace_ids").Array().IsEqual([]string{kb.DocID})
		obj.Value("metadata").Object().Value("doc_rev").String().IsEqual(inKB.Rev())
	})

	t.Run("the content of a file", func(t *testing.T) {
		get(t, rag.IndexFilesPath+inKB.DocID+"/content", inKB.DocID).Status(200).Body().IsEqual("alpha")
	})

	t.Run("a file outside every knowledge base has no state", func(t *testing.T) {
		get(t, rag.IndexFilesPath+outside.DocID, outside.DocID).Status(404)
	})

	t.Run("an unknown file has neither state nor content", func(t *testing.T) {
		get(t, rag.IndexFilesPath+"unknown", "unknown").Status(404)
		get(t, rag.IndexFilesPath+"unknown/content", "unknown").Status(404)
	})

	t.Run("the token of another file is refused", func(t *testing.T) {
		get(t, rag.IndexFilesPath+inKB.DocID, outside.DocID).Status(401)
		get(t, rag.IndexFilesPath+inKB.DocID+"/content", outside.DocID).Status(401)
	})

	t.Run("a request without token is refused", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		e.GET(rag.IndexFilesPath + inKB.DocID).Expect().Status(401)
	})

	t.Run("a trashed file has neither state nor content", func(t *testing.T) {
		trashed := writeIndexTestFile(t, fs, "/KB/c.txt", "gamma")
		_, err := vfs.TrashFile(fs, trashed)
		require.NoError(t, err)
		get(t, rag.IndexFilesPath+trashed.DocID, trashed.DocID).Status(404)
		get(t, rag.IndexFilesPath+trashed.DocID+"/content", trashed.DocID).Status(404)
	})
}

func writeIndexTestFile(t *testing.T, fs vfs.VFS, path, content string) *vfs.FileDoc {
	t.Helper()
	f, err := vfs.Create(fs, path)
	require.NoError(t, err)
	_, err = f.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	doc, err := fs.FileByPath(path)
	require.NoError(t, err)
	return doc
}
