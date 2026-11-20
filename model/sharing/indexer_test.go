package sharing

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSharingIndexerAntivirusScan(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	conf := config.GetConfig()
	previous, hadPrevious := conf.Contexts[config.DefaultInstanceContext]
	conf.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
		"antivirus": map[string]interface{}{"enabled": true},
	}
	t.Cleanup(func() {
		if hadPrevious {
			conf.Contexts[config.DefaultInstanceContext] = previous
		} else {
			delete(conf.Contexts, config.DefaultInstanceContext)
		}
	})

	write := func(t *testing.T, fs vfs.VFS, newdoc, olddoc *vfs.FileDoc, content string) {
		f, err := fs.CreateFile(newdoc, olddoc)
		require.NoError(t, err)
		_, err = io.Copy(f, strings.NewReader(content))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	t.Run("ReceivedNewFileIsPending", func(t *testing.T) {
		rev := "1-4fff5291a41bf1f493460d2070694c5a"
		indexer := newSharingIndexer(inst, &bulkRevs{
			Rev:       rev,
			Revisions: revsChainToStruct([]string{rev}),
		}, nil)
		fs := inst.VFS().UseSharingIndexer(indexer)

		doc, err := vfs.NewFileDoc("received-file", consts.RootDirID, -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		doc.DocID = uuidv7()
		write(t, fs, doc, nil, "received content")

		received, err := inst.VFS().FileByID(doc.DocID)
		require.NoError(t, err)
		require.NotNil(t, received.AntivirusScan)
		assert.Equal(t, vfs.AVStatusPending, received.AntivirusScan.Status)
	})

	t.Run("ReceivedContentIgnoresTheOriginVerdict", func(t *testing.T) {
		doc, err := vfs.NewFileDoc("changed-file", consts.RootDirID, -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		write(t, inst.VFS(), doc, nil, "local content")
		local, err := inst.VFS().FileByID(doc.DocID)
		require.NoError(t, err)

		indexer := newSharingIndexer(inst, &bulkRevs{
			Rev:       local.Rev(),
			Revisions: revsChainToStruct([]string{local.Rev()}),
		}, nil)
		indexer.IncrementRevision()
		fs := inst.VFS().UseSharingIndexer(indexer)

		changed, err := vfs.NewFileDoc("changed-file", consts.RootDirID, -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		changed.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusClean}
		write(t, fs, changed, local, "content changed on the origin")

		received, err := inst.VFS().FileByID(doc.DocID)
		require.NoError(t, err)
		require.NotNil(t, received.AntivirusScan)
		assert.Equal(t, vfs.AVStatusPending, received.AntivirusScan.Status)
	})

	t.Run("ReceivedRenameKeepsTheLocalVerdict", func(t *testing.T) {
		doc, err := vfs.NewFileDoc("infected-file", consts.RootDirID, -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		write(t, inst.VFS(), doc, nil, "infected content")

		local, err := inst.VFS().FileByID(doc.DocID)
		require.NoError(t, err)
		local.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusInfected, VirusName: "Eicar"}
		require.NoError(t, couchdb.UpdateDoc(inst, local))

		indexer := newSharingIndexer(inst, &bulkRevs{
			Rev:       local.Rev(),
			Revisions: revsChainToStruct([]string{local.Rev()}),
		}, nil)
		indexer.IncrementRevision()
		fs := inst.VFS().UseSharingIndexer(indexer)

		renamed := local.Clone().(*vfs.FileDoc)
		renamed.DocName = "renamed-file"
		renamed.ResetFullpath()
		renamed.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusClean}
		require.NoError(t, fs.UpdateFileDoc(local, renamed))

		received, err := inst.VFS().FileByID(doc.DocID)
		require.NoError(t, err)
		assert.Equal(t, "renamed-file", received.DocName)
		require.NotNil(t, received.AntivirusScan)
		assert.Equal(t, vfs.AVStatusInfected, received.AntivirusScan.Status)
		assert.Equal(t, "Eicar", received.AntivirusScan.VirusName)
	})
}
