package antivirus_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/notification"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/clamav"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/worker/antivirus"
	"github.com/stretchr/testify/require"
)

func TestAntivirus(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping ClamAV tests in short mode")
	}

	fixture := testutils.StartClamAV(t)
	clamavAddress := fixture.Address

	// ClamAV client tests can run in parallel
	t.Run("ClamAVClient", func(t *testing.T) {
		t.Run("CleanFile", func(t *testing.T) {
			t.Parallel()

			client := clamav.NewClient(clamavAddress, 30*time.Second, logger.WithNamespace("test"))

			// Test PING
			err := client.Ping(context.Background())
			require.NoError(t, err)

			// Test scanning a clean file
			cleanContent := []byte("This is a clean test file content")
			result, err := client.Scan(context.Background(), bytes.NewReader(cleanContent))
			require.NoError(t, err)
			require.Equal(t, clamav.StatusClean, result.Status)
			require.Empty(t, result.VirusName)
			require.Empty(t, result.Error)
		})

		t.Run("InfectedFile", func(t *testing.T) {
			t.Parallel()

			client := clamav.NewClient(clamavAddress, 30*time.Second, logger.WithNamespace("test"))

			// Test scanning an infected file (EICAR test signature)
			infectedContent := testutils.EICARTestSignature()
			result, err := client.Scan(context.Background(), bytes.NewReader(infectedContent))
			require.NoError(t, err)
			require.Equal(t, clamav.StatusInfected, result.Status)
			require.Contains(t, strings.ToUpper(result.VirusName), "EICAR")
			require.Empty(t, result.Error)
		})
	})

	// Worker tests share a single instance setup
	t.Run("Worker", func(t *testing.T) {
		config.UseTestFile(t)
		setup := testutils.NewSetup(t, "antivirus_worker_test")
		inst := setup.GetTestInstance(&lifecycle.Options{})

		// Configure antivirus in context for all worker tests
		conf := config.GetConfig()
		if conf.Contexts == nil {
			conf.Contexts = make(map[string]interface{})
		}
		conf.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
			"antivirus": map[string]interface{}{
				"enabled":       true,
				"address":       clamavAddress,
				"timeout":       30 * time.Second,
				"max_file_size": int64(0),
				"notifications": map[string]interface{}{
					"email_on_infected": true,
				},
			},
		}
		t.Cleanup(func() {
			delete(conf.Contexts, config.DefaultInstanceContext)
		})

		t.Run("CleanFile", func(t *testing.T) {
			testWorkerCleanFile(t, inst)
		})

		t.Run("InfectedFile", func(t *testing.T) {
			testWorkerInfectedFile(t, inst)
		})

		t.Run("SkipsLargeFiles", func(t *testing.T) {
			testWorkerSkipsLargeFiles(t, inst)
		})

		t.Run("DeletedFile", func(t *testing.T) {
			testWorkerDeletedFile(t, inst)
		})

		t.Run("Disabled", func(t *testing.T) {
			testWorkerDisabled(t, inst)
		})

		t.Run("FileVersionUpdate", func(t *testing.T) {
			testWorkerFileVersionUpdate(t, inst)
		})

		t.Run("VerdictOnlyForScannedContent", func(t *testing.T) {
			testVerdictOnlyForScannedContent(t, inst)
		})
	})
}

func testWorkerCleanFile(t *testing.T, inst *instance.Instance) {
	// Create a test file
	fs := inst.VFS()
	doc := createTestFile(t, fs, "clean-test.txt", []byte("This is a clean file"))
	defer deleteTestFile(t, fs, doc)

	// Create and run the job
	msg, err := job.NewMessage(&antivirus.Message{FileID: doc.DocID})
	require.NoError(t, err)

	j := &job.Job{
		Domain:     inst.Domain,
		WorkerType: "antivirus",
		Message:    msg,
	}

	ctx, cancel := job.NewTaskContext("test", j, inst)
	defer cancel()
	err = antivirus.Worker(ctx)
	require.NoError(t, err)

	// Verify the file was updated
	updatedDoc, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	require.NotNil(t, updatedDoc.AntivirusScan)
	require.Equal(t, vfs.AVStatusClean, updatedDoc.AntivirusScan.Status)
	require.NotNil(t, updatedDoc.AntivirusScan.ScannedAt)
	require.Empty(t, updatedDoc.AntivirusScan.VirusName)
}

func testWorkerInfectedFile(t *testing.T, inst *instance.Instance) {
	// Create a test file with EICAR signature
	fs := inst.VFS()
	doc := createTestFile(t, fs, "infected-test.txt", testutils.EICARTestSignature())
	defer deleteTestFile(t, fs, doc)

	// Create and run the job
	msg, err := job.NewMessage(&antivirus.Message{FileID: doc.DocID})
	require.NoError(t, err)

	j := &job.Job{
		Domain:     inst.Domain,
		WorkerType: "antivirus",
		Message:    msg,
	}

	ctx, cancel := job.NewTaskContext("test", j, inst)
	defer cancel()
	err = antivirus.Worker(ctx)
	require.NoError(t, err)

	// Verify the file was updated
	updatedDoc, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	require.NotNil(t, updatedDoc.AntivirusScan)
	require.Equal(t, vfs.AVStatusInfected, updatedDoc.AntivirusScan.Status)
	require.NotNil(t, updatedDoc.AntivirusScan.ScannedAt)
	require.Contains(t, strings.ToUpper(updatedDoc.AntivirusScan.VirusName), "EICAR")

	// Verify notification was sent
	verifyNotificationCreated(t, inst, "infected-test.txt")
}

func testWorkerSkipsLargeFiles(t *testing.T, inst *instance.Instance) {
	// Configure antivirus with a small max file size
	conf := config.GetConfig()
	ctxData := conf.Contexts[config.DefaultInstanceContext].(map[string]interface{})
	avData := ctxData["antivirus"].(map[string]interface{})
	originalMaxFileSize := avData["max_file_size"]
	avData["max_file_size"] = int64(10) // 10 bytes
	defer func() {
		avData["max_file_size"] = originalMaxFileSize
	}()

	// Create a test file larger than max size
	fs := inst.VFS()
	largeContent := []byte("This is a file larger than 10 bytes")
	doc := createTestFile(t, fs, "large-test.txt", largeContent)
	defer deleteTestFile(t, fs, doc)
	require.NotNil(t, doc.AntivirusScan)
	require.Equal(t, vfs.AVStatusSkipped, doc.AntivirusScan.Status)

	// Create and run the job
	msg, err := job.NewMessage(&antivirus.Message{FileID: doc.DocID})
	require.NoError(t, err)

	j := &job.Job{
		Domain:     inst.Domain,
		WorkerType: "antivirus",
		Message:    msg,
	}

	ctx, cancel := job.NewTaskContext("test", j, inst)
	defer cancel()
	err = antivirus.Worker(ctx)
	require.NoError(t, err)

	// Verify the file was marked as skipped
	updatedDoc, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	require.NotNil(t, updatedDoc.AntivirusScan)
	require.Equal(t, vfs.AVStatusSkipped, updatedDoc.AntivirusScan.Status)
	require.Equal(t, doc.Rev(), updatedDoc.Rev())

	// Verify notification was sent
	verifyNotificationCreated(t, inst, "large-test.txt")
}

func testWorkerDeletedFile(t *testing.T, inst *instance.Instance) {
	// Create and run the job for a non-existent file
	msg, err := job.NewMessage(&antivirus.Message{FileID: "non-existent-file-id"})
	require.NoError(t, err)

	j := &job.Job{
		Domain:     inst.Domain,
		WorkerType: "antivirus",
		Message:    msg,
	}

	ctx, cancel := job.NewTaskContext("test", j, inst)
	defer cancel()
	err = antivirus.Worker(ctx)
	// Should not error - just skip
	require.NoError(t, err)
}

func testWorkerDisabled(t *testing.T, inst *instance.Instance) {
	// Temporarily disable antivirus
	originalEnabled := antivirusEnabledValue(t)
	t.Cleanup(func() {
		setAntivirusEnabled(t, originalEnabled)
	})
	setAntivirusEnabled(t, false)

	// Create a test file
	fs := inst.VFS()
	doc := createTestFile(t, fs, "disabled-test.txt", []byte("test content"))
	defer deleteTestFile(t, fs, doc)

	// Create and run the job
	msg, err := job.NewMessage(&antivirus.Message{FileID: doc.DocID})
	require.NoError(t, err)

	j := &job.Job{
		Domain:     inst.Domain,
		WorkerType: "antivirus",
		Message:    msg,
	}

	ctx, cancel := job.NewTaskContext("test", j, inst)
	defer cancel()
	err = antivirus.Worker(ctx)
	require.NoError(t, err)

	// Verify the file was not updated (no scan status)
	updatedDoc, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	require.Nil(t, updatedDoc.AntivirusScan)
}

func createTestFile(t *testing.T, fs vfs.VFS, name string, content []byte) *vfs.FileDoc {
	t.Helper()
	parent, err := fs.DirByPath("/")
	require.NoError(t, err)
	doc, err := vfs.NewFileDoc(name, parent.DocID, int64(len(content)), nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	file, err := fs.CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = file.Write(content)
	require.NoError(t, err)
	err = file.Close()
	require.NoError(t, err)
	updatedDoc, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	return updatedDoc
}

func deleteTestFile(t *testing.T, fs vfs.VFS, doc *vfs.FileDoc) {
	t.Helper()
	_ = fs.DestroyFile(doc)
}

func verifyNotificationCreated(t *testing.T, inst *instance.Instance, fileName string) {
	t.Helper()
	require.True(t, hasNotification(t, inst, fileName), "Expected notification for file %s not found", fileName)
}

func hasNotification(t *testing.T, inst *instance.Instance, fileName string) bool {
	t.Helper()
	var notifs []*notification.Notification
	err := couchdb.GetAllDocs(inst, consts.Notifications, nil, &notifs)
	require.NoError(t, err)

	for _, n := range notifs {
		if n.Category == "antivirus-alert" {
			if data, ok := n.Data["FileName"].(string); ok && data == fileName {
				return true
			}
		}
	}
	return false
}

// testVerdictOnlyForScannedContent tests that a verdict computed for a content
// is not written on a file whose content changed during the scan.
func testVerdictOnlyForScannedContent(t *testing.T, inst *instance.Instance) {
	fs := inst.VFS()
	originalEnabled := antivirusEnabledValue(t)
	t.Cleanup(func() {
		setAntivirusEnabled(t, originalEnabled)
	})
	// No background scan job must write a verdict during this test.
	setAntivirusEnabled(t, false)

	scanned := createTestFile(t, fs, "stale-verdict-test.txt", []byte("This is the scanned content"))
	defer deleteTestFile(t, fs, scanned)

	// A new content is uploaded during the scan, and gets its own pending status.
	current := updateTestFileContent(t, fs, scanned, []byte("This is the content uploaded during the scan"))
	require.NotEqual(t, scanned.MD5Sum, current.MD5Sum)
	pending := current.Clone().(*vfs.FileDoc)
	pending.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusPending}
	require.NoError(t, couchdb.UpdateDoc(fs, pending))
	current, err := fs.FileByID(scanned.DocID)
	require.NoError(t, err)

	verdict := &vfs.AntivirusScan{Status: vfs.AVStatusInfected, VirusName: "Stale"}
	err = antivirus.UpdateScanStatus(inst, scanned, verdict)
	require.NoError(t, err)

	got, err := fs.FileByID(scanned.DocID)
	require.NoError(t, err)
	require.NotNil(t, got.AntivirusScan)
	require.Equal(t, vfs.AVStatusPending, got.AntivirusScan.Status)
	require.Equal(t, current.Rev(), got.Rev())
	require.False(t, hasNotification(t, inst, "stale-verdict-test.txt"))

	// The verdict on the current content is still written.
	err = antivirus.UpdateScanStatus(inst, current, verdict)
	require.NoError(t, err)

	got, err = fs.FileByID(scanned.DocID)
	require.NoError(t, err)
	require.NotNil(t, got.AntivirusScan)
	require.Equal(t, vfs.AVStatusInfected, got.AntivirusScan.Status)
	verifyNotificationCreated(t, inst, "stale-verdict-test.txt")
}

// testWorkerFileVersionUpdate tests that when a file is updated with new content,
// the antivirus worker properly scans the new version.
func testWorkerFileVersionUpdate(t *testing.T, inst *instance.Instance) {
	fs := inst.VFS()
	originalEnabled := antivirusEnabledValue(t)
	t.Cleanup(func() {
		setAntivirusEnabled(t, originalEnabled)
	})

	// Step 1: Create a clean file and scan it
	setAntivirusEnabled(t, false)
	doc := createTestFile(t, fs, "version-test.txt", []byte("This is clean content"))
	defer deleteTestFile(t, fs, doc)

	// Run initial scan
	setAntivirusEnabled(t, true)
	msg, err := job.NewMessage(&antivirus.Message{FileID: doc.DocID})
	require.NoError(t, err)

	j := &job.Job{
		Domain:     inst.Domain,
		WorkerType: "antivirus",
		Message:    msg,
	}

	ctx, cancel := job.NewTaskContext("test", j, inst)
	err = antivirus.Worker(ctx)
	cancel()
	require.NoError(t, err)

	// Verify file is clean
	doc, err = fs.FileByID(doc.DocID)
	require.NoError(t, err)
	require.NotNil(t, doc.AntivirusScan)
	require.Equal(t, vfs.AVStatusClean, doc.AntivirusScan.Status)
	originalMD5 := doc.MD5Sum

	// Step 2: Update file with infected content (simulating new version upload)
	setAntivirusEnabled(t, false)
	doc = updateTestFileContent(t, fs, doc, testutils.EICARTestSignature())

	// Verify MD5 changed (content was updated)
	require.NotEqual(t, originalMD5, doc.MD5Sum, "MD5 should change when content is updated")

	// Step 3: Run scan again on the updated file
	setAntivirusEnabled(t, true)
	msg, err = job.NewMessage(&antivirus.Message{FileID: doc.DocID})
	require.NoError(t, err)

	j = &job.Job{
		Domain:     inst.Domain,
		WorkerType: "antivirus",
		Message:    msg,
	}

	ctx, cancel = job.NewTaskContext("test", j, inst)
	err = antivirus.Worker(ctx)
	cancel()
	require.NoError(t, err)

	// Step 4: Verify the new version is detected as infected
	updatedDoc, err := fs.FileByID(doc.DocID)
	require.NoError(t, err)
	require.NotNil(t, updatedDoc.AntivirusScan)
	require.Equal(t, vfs.AVStatusInfected, updatedDoc.AntivirusScan.Status)
	require.Contains(t, strings.ToUpper(updatedDoc.AntivirusScan.VirusName), "EICAR")
}

// updateTestFileContent updates a file's content by creating a new version.
// It retries on conflict errors caused by background antivirus jobs updating the document.
func updateTestFileContent(t *testing.T, fs vfs.VFS, olddoc *vfs.FileDoc, newContent []byte) *vfs.FileDoc {
	t.Helper()

	var result *vfs.FileDoc
	fileID := olddoc.DocID

	err := utils.RetryWithExpBackoff(5, 10*time.Millisecond, func() error {
		// Re-fetch the document to get the latest revision
		currentDoc, err := fs.FileByID(fileID)
		if err != nil {
			return err
		}

		// Create new file doc for update
		newdoc, err := vfs.NewFileDoc(
			currentDoc.DocName,
			currentDoc.DirID,
			-1,  // size will be calculated
			nil, // MD5 will be calculated
			currentDoc.Mime,
			currentDoc.Class,
			time.Now(),
			currentDoc.Executable,
			currentDoc.Trashed,
			currentDoc.Encrypted,
			currentDoc.Tags,
		)
		if err != nil {
			return err
		}

		// Create the new version of the file
		file, err := fs.CreateFile(newdoc, currentDoc)
		if err != nil {
			return err
		}

		if _, err = file.Write(newContent); err != nil {
			return err
		}

		if err = file.Close(); err != nil {
			return err
		}

		// Success - get the updated document
		result, err = fs.FileByID(fileID)
		return err
	})

	require.NoError(t, err)
	return result
}

func antivirusEnabledValue(t *testing.T) bool {
	t.Helper()

	conf := config.GetConfig()
	ctxData := conf.Contexts[config.DefaultInstanceContext].(map[string]interface{})
	avData := ctxData["antivirus"].(map[string]interface{})
	return avData["enabled"].(bool)
}

func setAntivirusEnabled(t *testing.T, enabled bool) {
	t.Helper()

	conf := config.GetConfig()
	ctxData := conf.Contexts[config.DefaultInstanceContext].(map[string]interface{})
	avData := ctxData["antivirus"].(map[string]interface{})
	avData["enabled"] = enabled
}
