package antivirus

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/notification"
	"github.com/cozy/cozy-stack/model/notification/center"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/clamav"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "antivirus",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 3,
		Timeout:      10 * time.Minute,
		WorkerFunc:   Worker,
	})
}

// Message is the structure of the antivirus job message
type Message struct {
	FileID string `json:"file_id"`
}

// Worker is the antivirus worker function
func Worker(ctx *job.TaskContext) error {
	var msg Message
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}

	inst := ctx.Instance
	log := inst.Logger().WithNamespace("antivirus")

	log.Debugf("Starting antivirus job for file %s", msg.FileID)

	// Check if antivirus is enabled for this instance
	if !isEnabledForInstance(inst) {
		log.Debugf("Antivirus is disabled for context %s, skipping scan", inst.ContextName)
		return nil
	}

	log.Infof("Scanning file %s", msg.FileID)

	// Get the file document
	fs := inst.VFS()
	file, err := fs.FileByID(msg.FileID)
	if err != nil {
		if os.IsNotExist(err) || couchdb.IsNotFoundError(err) {
			log.Debugf("File %s not found (possibly deleted), skipping scan", msg.FileID)
			return nil // File was deleted, nothing to do
		}
		log.Errorf("Failed to get file %s: %v", msg.FileID, err)
		return err
	}

	log.Debugf("File %s: name=%s, size=%d bytes, mime=%s", msg.FileID, file.DocName, file.ByteSize, file.Mime)

	avConfig := config.GetAntivirusConfig(inst.ContextName)

	if avConfig.MaxFileSize > 0 && file.ByteSize > avConfig.MaxFileSize {
		log.Infof("File %s is too large (%d bytes > %d max), skipping scan", msg.FileID, file.ByteSize, avConfig.MaxFileSize)
		scan := &vfs.AntivirusScan{Status: vfs.AVStatusSkipped}
		// The VFS writes skipped with the content: rewriting it would only
		// add a revision, but the user must still be told.
		if file.AntivirusScan != nil && file.AntivirusScan.Status == vfs.AVStatusSkipped {
			sendNotification(inst, file, scan.Status, getIssueDescription(inst, scan))
			return nil
		}
		return updateScanStatus(inst, file, scan)
	}

	// Create ClamAV client
	timeout := avConfig.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	log.Debugf("Creating ClamAV client: address=%s, timeout=%v", avConfig.Address, timeout)
	client := clamav.NewClient(avConfig.Address, timeout, log)

	log.Debugf("Opening file %s for scanning", msg.FileID)
	content, err := fs.OpenFile(file)
	if err != nil {
		log.Errorf("Failed to open file %s: %v", msg.FileID, err)
		return updateScanStatus(inst, file, &vfs.AntivirusScan{
			Status: vfs.AVStatusError,
			Error:  err.Error(),
		})
	}
	defer content.Close()

	// Scan the file
	log.Debugf("Sending file %s to ClamAV for scanning", msg.FileID)
	result, err := client.Scan(ctx, content)
	if err != nil {
		log.Errorf("Failed to scan the file %s: %v", msg.FileID, err)
		return updateScanStatus(inst, file, &vfs.AntivirusScan{
			Status: vfs.AVStatusError,
			Error:  err.Error(),
		})
	}

	// Update file with scan result
	now := time.Now()
	st := &vfs.AntivirusScan{
		ScannedAt: &now,
	}

	switch result.Status {
	case clamav.StatusClean:
		log.Debugf("File %s is clean", msg.FileID)
		st.Status = vfs.AVStatusClean
	case clamav.StatusInfected:
		log.Warnf("File %s is infected with %s", msg.FileID, result.VirusName)
		st.Status = vfs.AVStatusInfected
		st.VirusName = result.VirusName
	default:
		log.Errorf("Scan error for file %s: %s", msg.FileID, result.Error)
		st.Status = vfs.AVStatusError
		st.Error = result.Error
	}

	log.Debugf("Updating scan status for file %s: status=%s", msg.FileID, st.Status)
	return updateScanStatus(inst, file, st)
}

// updateScanStatus updates the file document with the scan status.
// It retries on conflict errors to handle race conditions with the trigger.
// It also sends a notification when the status is infected, skipped, or error.
func updateScanStatus(inst *instance.Instance, file *vfs.FileDoc, scan *vfs.AntivirusScan) error {
	const maxRetries = 3
	var err error
	fs := inst.VFS()

	for i := 0; i < maxRetries; i++ {
		var f *vfs.FileDoc
		f, err = fs.FileByID(file.DocID)
		if err != nil {
			return err
		}
		newdoc := f.Clone().(*vfs.FileDoc)
		newdoc.AntivirusScan = scan
		err = couchdb.UpdateDoc(fs, newdoc)
		if err == nil {
			// Send notification for non-clean statuses
			if scan.Status != vfs.AVStatusClean && scan.Status != vfs.AVStatusPending {
				issueDesc := getIssueDescription(inst, scan)
				sendNotification(inst, file, scan.Status, issueDesc)
			}
			return nil
		}
		if !couchdb.IsConflictError(err) {
			return err
		}
		// Conflict error, retry with fresh document
	}

	return err
}

// isEnabledForInstance returns true if antivirus is enabled for the given instance
func isEnabledForInstance(inst *instance.Instance) bool {
	cfg := config.GetAntivirusConfig(inst.ContextName)
	return cfg != nil && cfg.Enabled
}

// sendNotification sends a notification to the user about an antivirus issue
// using the notification center.
func sendNotification(inst *instance.Instance, file *vfs.FileDoc, status string, issueDescription string) {
	avConfig := config.GetAntivirusConfig(inst.ContextName)
	if avConfig == nil || !avConfig.Notifications.EmailOnInfected {
		return
	}

	log := inst.Logger().WithNamespace("antivirus")

	driveURL := inst.SubDomain("drive")
	driveURL.Fragment = fmt.Sprintf("/folder/%s/file/%s", file.DirID, file.DocID)
	fileURL := driveURL.String()

	n := &notification.Notification{
		Title: inst.Translate("Mail Antivirus Alert Title"),
		Slug:  consts.DriveSlug,
		Data: map[string]interface{}{
			"IssueDescription": issueDescription,
			"FileName":         file.DocName,
			"FileURL":          fileURL,
		},
		PreferredChannels: []string{"mail"},
	}

	err := center.PushStack(inst.DomainName(), center.NotificationAntivirusAlert, n)
	if err != nil {
		log.Errorf("Failed to push antivirus notification: %v", err)
		return
	}

	log.Infof("Sent antivirus notification for file %s (status: %s)", file.DocID, status)
	return
}

// getIssueDescription returns a translated issue description for the given status.
func getIssueDescription(inst *instance.Instance, scan *vfs.AntivirusScan) string {
	switch scan.Status {
	case vfs.AVStatusInfected:
		if scan.VirusName != "" {
			return inst.Translate("Mail Antivirus Issue Infected") + " (" + scan.VirusName + ")"
		}
		return inst.Translate("Mail Antivirus Issue Infected")
	case vfs.AVStatusSkipped:
		return inst.Translate("Mail Antivirus Issue Skipped")
	case vfs.AVStatusError:
		if scan.Error != "" {
			return inst.Translate("Mail Antivirus Issue Error") + ": " + scan.Error
		}
		return inst.Translate("Mail Antivirus Issue Error")
	default:
		return ""
	}
}
