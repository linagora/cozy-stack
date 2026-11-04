// Package share is where the workers for Cozy to Cozy sharings are defined.
package share

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   sharing.RevocationWorker,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 5,
		Reserved:     true,
		RetryDelay:   time.Second,
		Timeout:      5 * time.Minute,
		WorkerFunc:   WorkerRevoke,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "share-group",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      30 * time.Second,
		WorkerFunc:   WorkerGroup,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "share-track",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      30 * time.Second,
		WorkerFunc:   WorkerTrack,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "share-autoaccept",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 4,
		Reserved:     true,
		RetryDelay:   250 * time.Millisecond,
		Timeout:      30 * time.Second,
		WorkerFunc:   WorkerAutoAccept,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:  "share-replicate",
		Concurrency: runtime.NumCPU(),
		// XXX the worker is not idempotent: if it fails, it adds a new job to
		// retry, but with MaxExecCount > 1, it can amplifies a lot the number
		// of retries
		MaxExecCount: 1,
		Reserved:     true,
		Timeout:      5 * time.Minute,
		WorkerFunc:   WorkerReplicate,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:  "share-upload",
		Concurrency: runtime.NumCPU(),
		// XXX the worker is not idempotent: if it fails, it adds a new job to
		// retry, but with MaxExecCount > 1, it can amplifies a lot the number
		// of retries
		MaxExecCount: 1,
		Reserved:     true,
		Timeout:      1 * time.Hour,
		WorkerFunc:   WorkerUpload,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "share-update",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      30 * time.Second,
		WorkerFunc:   WorkerUpdate,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   sharing.CollectedContactsWorker,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 5,
		Reserved:     true,
		RetryDelay:   time.Second,
		Timeout:      30 * time.Second,
		WorkerFunc:   WorkerCollectedContacts,
	})
}

// WorkerRevoke delivers a batch of recipient revocation notifications.
func WorkerRevoke(ctx *job.TaskContext) error {
	var msgJob sharing.RevocationJob
	if err := ctx.UnmarshalMessage(&msgJob); err != nil {
		return err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(msgJob.Encrypted)
	if err != nil {
		return err
	}
	key := config.GetKeyring().CredentialsDecryptorKey()
	if key == nil {
		return account.ErrBadCredentials
	}
	plaintext, err := account.DecryptBufferWithKey(key, ciphertext)
	if err != nil {
		return err
	}
	var msg sharing.RevocationMsg
	if err := json.Unmarshal(plaintext, &msg); err != nil {
		return err
	}
	return sharing.NotifyRevocations(ctx, ctx.Instance, &msg)
}

// WorkerGroup is used to update the list of members of sharings for a group
// when someone is added or removed to this group.
func WorkerGroup(ctx *job.TaskContext) error {
	var msg job.ShareGroupMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	ctx.Instance.Logger().WithNamespace("share").
		Debugf("Group %#v", msg)
	return sharing.UpdateGroups(ctx.Instance, msg)
}

// WorkerTrack is used to update the io.cozy.shared database when a document
// that matches a sharing rule is created/updated/remove
func WorkerTrack(ctx *job.TaskContext) error {
	var msg sharing.TrackMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	var evt sharing.TrackEvent
	if err := ctx.UnmarshalEvent(&evt); err != nil {
		return err
	}
	ctx.Instance.Logger().WithNamespace("share").
		Debugf("Track %#v - %#v", msg, evt)
	return sharing.UpdateShared(ctx.Instance, msg, evt)
}

// WorkerAutoAccept handles auto-acceptance of drive sharings from trusted senders
func WorkerAutoAccept(ctx *job.TaskContext) error {
	var msg sharing.AutoAcceptMsg
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	ctx.Instance.Logger().WithNamespace("share").
		Debugf("AutoAccept drive sharing %s", msg.SharingID)
	return sharing.HandleAutoAccept(ctx.Instance, &msg)
}

// WorkerCollectedContacts publishes the contacts created by a sharing.
func WorkerCollectedContacts(ctx *job.TaskContext) error {
	var msg sharing.CollectedContactsMsg
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	ctx.Instance.Logger().WithNamespace("share").
		Debugf("CollectedContacts %d contacts", len(msg.Emails))
	err := sharing.PublishCollectedContacts(ctx, ctx.Instance, ctx.Publisher, msg.Emails)
	// No retry: RabbitMQ is not configured.
	if errors.Is(err, rabbitmq.ErrNotConfigured) {
		ctx.SetNoRetry()
	}
	return err
}

// WorkerReplicate is used for the replication of documents to the other
// members of a sharing.
func WorkerReplicate(ctx *job.TaskContext) error {
	var msg sharing.ReplicateMsg
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	ctx.Instance.Logger().WithNamespace("share").
		Debugf("Replicate %#v", msg)
	s, err := sharing.FindSharing(ctx.Instance, msg.SharingID)
	if err != nil {
		return err
	}
	if !s.Active {
		return nil
	}
	return s.Replicate(ctx.Instance, msg.Errors)
}

// WorkerUpload is used to upload files for a sharing
func WorkerUpload(ctx *job.TaskContext) error {
	var msg sharing.UploadMsg
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	ctx.Instance.Logger().WithNamespace("share").
		Debugf("Upload %#v", msg)
	s, err := sharing.FindSharing(ctx.Instance, msg.SharingID)
	if err != nil {
		return err
	}
	if !s.Active {
		return nil
	}
	return s.Upload(ctx.Instance, ctx.Context, msg.Errors)
}

func WorkerUpdate(ctx *job.TaskContext) error {
	var msg sharing.UpdateMsg
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	ctx.Instance.Logger().WithNamespace("share").
		Debugf("Update %#v", msg)

	s, err := sharing.FindSharing(ctx.Instance, msg.SharingID)
	if err != nil {
		return err
	}
	if !s.Active {
		ctx.Instance.Logger().WithNamespace("share").
			Debugf("Skipping description update for inactive sharing %s", msg.SharingID)
		return nil
	}
	return s.PatchDescription(ctx.Instance, msg.NewDescription)
}
