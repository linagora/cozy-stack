package sharing

import (
	"errors"
	"os"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/shortcut"
)

// AutoAcceptMsg is the payload for auto-accepting a drive sharing from a trusted sender
type AutoAcceptMsg struct {
	SharingID string `json:"sharing_id"`
	State     string `json:"state"`
}

// EnqueueAutoAccept schedules a job to auto-accept a drive sharing
func EnqueueAutoAccept(inst *instance.Instance, sharingID, state string) error {
	if inst == nil || sharingID == "" || state == "" {
		return ErrInvalidSharing
	}

	msg, err := job.NewMessage(&AutoAcceptMsg{
		SharingID: sharingID,
		State:     state,
	})
	if err != nil {
		return err
	}

	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "share-autoaccept",
		Message:    msg,
	})
	return err
}

// HandleAutoAccept executes the auto-acceptance for a Drive sharing.
// The OAuth state must be provided by the owner in the sharing request.
func HandleAutoAccept(inst *instance.Instance, msg *AutoAcceptMsg) error {
	if inst == nil || msg == nil || msg.SharingID == "" || msg.State == "" {
		return ErrInvalidSharing
	}

	s, err := FindSharing(inst, msg.SharingID)
	if err != nil {
		return err
	}

	if !s.Active {
		if err := s.SendAnswer(inst, msg.State); err != nil {
			if errors.Is(err, ErrAlreadyAccepted) {
				return nil
			}
			return err
		}
	}
	if s.ShortcutID == "" {
		return nil
	}

	fs := inst.VFS()
	fileDoc, err := fs.FileByID(s.ShortcutID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isReusableShortcut(fileDoc) {
		return nil
	}

	// Accepting a sharing does not mean the user has seen its shortcut.
	body := shortcut.Generate(s.DriveTargetURL(inst).String())
	updated := fileDoc.Clone().(*vfs.FileDoc)
	updated.ByteSize = int64(len(body))
	updated.MD5Sum = nil
	return writeShortcutFile(fs, updated, fileDoc, body)
}
