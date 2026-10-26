package move

import (
	"archive/zip"
	"bytes"
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportSkipsRecents(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{
		ExportDataDir + "/" + consts.FilesRecents + "/source-id:file-id.json": `{
			"_id": "source-id:file-id",
			"_rev": "1-abc",
			"source_locator": "alice.example.net/file-id",
			"last_activity_at": "2026-10-26T10:00:00Z",
			"schema_version": 1
		}`,
		ExportDataDir + "/io.cozy.foos/foo-id.json": `{"_id": "foo-id", "_rev": "1-abc"}`,
	}
	for name, content := range entries {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)

	im := &importer{
		inst:            inst,
		fs:              inst.VFS(),
		servicesInError: make(map[string]bool),
	}
	require.NoError(t, im.importZip(zr))

	var doc couchdb.JSONDoc
	err = couchdb.GetDoc(inst, consts.FilesRecents, "source-id:file-id", &doc)
	assert.True(t, couchdb.IsNotFoundError(err), "the recents must not be imported, got %v", err)
	assert.NoError(t, couchdb.GetDoc(inst, "io.cozy.foos", "foo-id", &doc),
		"the other documents of the archive are imported")
}
