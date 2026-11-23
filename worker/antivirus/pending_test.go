package antivirus_test

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/worker/antivirus"
	"github.com/stretchr/testify/require"
)

func TestPushPendingScans(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping ClamAV tests in short mode")
	}

	fixture := testutils.StartClamAV(t)

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{})

	conf := config.GetConfig()
	if conf.Contexts == nil {
		conf.Contexts = make(map[string]interface{})
	}
	conf.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
		"antivirus": map[string]interface{}{
			"enabled":       true,
			"address":       fixture.Address,
			"timeout":       30 * time.Second,
			"max_file_size": int64(0),
		},
	}
	t.Cleanup(func() {
		delete(conf.Contexts, config.DefaultInstanceContext)
	})

	fs := inst.VFS()
	longAgo := time.Now().Add(-2 * time.Hour)

	oldPending := createScannedFile(t, inst, "old-pending.txt")
	setScanState(t, inst, oldPending, &vfs.AntivirusScan{Status: vfs.AVStatusPending}, longAgo)
	defer deleteTestFile(t, fs, oldPending)

	recentPending := createScannedFile(t, inst, "recent-pending.txt")
	setScanState(t, inst, recentPending, &vfs.AntivirusScan{Status: vfs.AVStatusPending}, time.Now())
	defer deleteTestFile(t, fs, recentPending)

	oldClean := createScannedFile(t, inst, "old-clean.txt")
	setScanState(t, inst, oldClean, &vfs.AntivirusScan{Status: vfs.AVStatusClean}, longAgo)
	defer deleteTestFile(t, fs, oldClean)

	oldUnscanned := createScannedFile(t, inst, "old-unscanned.txt")
	setScanState(t, inst, oldUnscanned, nil, longAgo)
	defer deleteTestFile(t, fs, oldUnscanned)

	before := time.Now().Add(-time.Hour)

	t.Run("Disabled", func(t *testing.T) {
		setAntivirusEnabled(t, false)
		defer setAntivirusEnabled(t, true)

		ids, err := antivirus.PushPendingScans(inst, before)
		require.NoError(t, err)
		require.Empty(t, ids)
	})

	t.Run("Enabled", func(t *testing.T) {
		ids, err := antivirus.PushPendingScans(inst, before)
		require.NoError(t, err)
		require.Equal(t, []string{oldPending.DocID}, ids)

		require.Eventually(t, func() bool {
			doc, err := fs.FileByID(oldPending.DocID)
			return err == nil && doc.AntivirusScan != nil &&
				doc.AntivirusScan.Status == vfs.AVStatusClean
		}, 30*time.Second, 100*time.Millisecond)
	})
}

// createScannedFile creates a file and waits for the antivirus trigger and
// worker to give it a verdict, so that they do not touch it afterwards.
func createScannedFile(t *testing.T, inst *instance.Instance, name string) *vfs.FileDoc {
	t.Helper()
	fs := inst.VFS()
	doc := createTestFile(t, fs, name, []byte("This is a clean file"))
	require.Eventually(t, func() bool {
		f, err := fs.FileByID(doc.DocID)
		return err == nil && f.AntivirusScan != nil &&
			f.AntivirusScan.Status == vfs.AVStatusClean
	}, 30*time.Second, 100*time.Millisecond)
	return doc
}

// setScanState writes the antivirus status and the update date of a file
// without changing its content, so the trigger does not rescan it.
func setScanState(t *testing.T, inst *instance.Instance, doc *vfs.FileDoc, scan *vfs.AntivirusScan, updatedAt time.Time) {
	t.Helper()
	fs := inst.VFS()
	f, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	newdoc := f.Clone().(*vfs.FileDoc)
	newdoc.AntivirusScan = scan
	newdoc.UpdatedAt = updatedAt
	require.NoError(t, couchdb.UpdateDoc(fs, newdoc))
}
