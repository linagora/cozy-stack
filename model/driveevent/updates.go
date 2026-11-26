package driveevent

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/messaging"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/redis/go-redis/v9"
)

// A file can be saved every few seconds, by a sync or an editor. Like the
// digest of shared drives (ADR 010), its updates are grouped in a window that
// the first one opens: at its end, one event tells the file as it is then.

// WorkerUpdates publishes the update of a file at the end of its window.
const WorkerUpdates = "drive-file-updated"

const defaultWindow = 30 * time.Second

// keepByOwner is how long the job can find who made the updates of a window,
// retries included.
const keepByOwner = time.Hour

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   WorkerUpdates,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 5,
		Reserved:     true,
		Timeout:      time.Minute,
		WorkerFunc:   workUpdates,
	})
}

// UpdatesMessage is the message of the drive-file-updated jobs.
type UpdatesMessage struct {
	FileID string `json:"file_id"`
	// Window is when the window opened, in milliseconds: it names the window.
	Window string `json:"window"`
}

// windowOf is the window of the context, the one of the digest of shared
// drives.
func windowOf(inst *instance.Instance) time.Duration {
	settings, ok := inst.SettingsContext()
	if !ok {
		return defaultWindow
	}
	cfg, _ := settings["drive_activity"].(map[string]interface{})
	w, _ := cfg["window"].(string)
	if d, err := time.ParseDuration(w); err == nil && d > 0 {
		return d
	}
	return defaultWindow
}

// windowKey holds the open window of the file, which expires with it.
func windowKey(inst *instance.Instance, fileID string) string {
	return "drive-event:" + inst.DBPrefix() + "/" + fileID + ":window"
}

// byOwnerKey is set when the owner made one of the updates of the window.
func byOwnerKey(inst *instance.Instance, fileID, window string) string {
	return "drive-event:" + inst.DBPrefix() + "/" + fileID + ":" + window + ":by-owner"
}

// scheduleUpdates schedules the job that publishes the updates of the file at
// the end of the window. A variable, so that tests can replace the jobs.
var scheduleUpdates = func(inst *instance.Instance, msg *UpdatesMessage, window time.Duration) error {
	m, err := job.NewMessage(msg)
	if err != nil {
		return err
	}
	t, err := job.NewTrigger(inst, job.TriggerInfos{
		Type:       "@in",
		WorkerType: WorkerUpdates,
		Arguments:  window.String(),
	}, m)
	if err != nil {
		return err
	}
	return job.System().AddTrigger(t)
}

// groupUpdate records an update of the file in its window, and opens one when
// there is none. It returns false when the update must be published at once:
// the Stack has no Redis, or the window could not be opened.
func groupUpdate(inst *instance.Instance, file *vfs.FileDoc, byOwner bool) bool {
	cli := config.GetConfig().SessionStorage
	if cli == nil {
		return false
	}
	ctx := context.Background()
	window := windowOf(inst)
	log := inst.Logger().WithNamespace("drive-event")
	key := windowKey(inst, file.DocID)
	start := strconv.FormatInt(time.Now().UnixMilli(), 10)
	opened, err := cli.SetNX(ctx, key, start, window).Result()
	if err != nil {
		log.Warnf("Cannot group the updates of %s: %s", file.DocID, err)
		return false
	}
	w := start
	if !opened {
		if w, err = cli.Get(ctx, key).Result(); err != nil {
			// The window closed in between: this update opens the next one.
			if errors.Is(err, redis.Nil) {
				return groupUpdate(inst, file, byOwner)
			}
			log.Warnf("Cannot group the updates of %s: %s", file.DocID, err)
			return false
		}
	}
	if byOwner {
		if err := cli.Set(ctx, byOwnerKey(inst, file.DocID, w), "1", keepByOwner).Err(); err != nil {
			log.Warnf("Cannot record an update of %s: %s", file.DocID, err)
		}
	}
	if opened {
		msg := &UpdatesMessage{FileID: file.DocID, Window: w}
		if err := scheduleUpdates(inst, msg, window); err != nil {
			log.Warnf("Cannot schedule the update of %s: %s", file.DocID, err)
			// Without a job, the window would swallow the next updates.
			cli.Del(ctx, key, byOwnerKey(inst, file.DocID, w))
			return false
		}
	}
	return true
}

func workUpdates(ctx *job.TaskContext) error {
	var msg UpdatesMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	pub := ctx.Publisher
	if pub == nil {
		pub = publisher
	}
	return publishUpdates(ctx, pub, ctx.Instance, &msg)
}

// publishUpdates publishes the file as it is at the end of its window,
// nothing when it is gone or in the trash: its deletion tells it. A failure is
// returned, so that the job is tried again.
func publishUpdates(ctx context.Context, pub messaging.Publisher, inst *instance.Instance, msg *UpdatesMessage) error {
	cli := config.GetConfig().SessionStorage
	if cli != nil {
		// The scheduler rounds the time of the job to the second, so the key
		// may outlive it: an update from now on must open the next window.
		if err := closeWindow(ctx, cli, inst, msg); err != nil {
			return err
		}
	}
	if pub == nil || !FileEventsOn(inst) {
		return forgetByOwner(ctx, cli, inst, msg)
	}
	file, err := inst.VFS().FileByID(msg.FileID)
	if errors.Is(err, os.ErrNotExist) || (err == nil && file.Trashed) {
		return forgetByOwner(ctx, cli, inst, msg)
	}
	if err != nil {
		return err
	}
	byOwner := false
	if cli != nil {
		n, err := cli.Get(ctx, byOwnerKey(inst, msg.FileID, msg.Window)).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		byOwner = n == "1"
	}
	if err := publishWith(ctx, pub, inst, updatedEvent(file), fileObject(inst, file), byOwner); err != nil {
		return err
	}
	return forgetByOwner(ctx, cli, inst, msg)
}

// closeWindow deletes the window of the message, unless a next one has
// already replaced it.
func closeWindow(ctx context.Context, cli redis.UniversalClient, inst *instance.Instance, msg *UpdatesMessage) error {
	key := windowKey(inst, msg.FileID)
	w, err := cli.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) || (err == nil && w != msg.Window) {
		return nil
	}
	if err != nil {
		return err
	}
	return cli.Del(ctx, key).Err()
}

func forgetByOwner(ctx context.Context, cli redis.UniversalClient, inst *instance.Instance, msg *UpdatesMessage) error {
	if cli == nil {
		return nil
	}
	return cli.Del(ctx, byOwnerKey(inst, msg.FileID, msg.Window)).Err()
}

func updatedEvent(file *vfs.FileDoc) rabbitmq.CloudEvent {
	// A file is updated many times: its revision tells the updates apart.
	return rabbitmq.CloudEvent{
		ID:   file.DocID + "/updated/" + file.DocRev,
		Type: rabbitmq.RoutingKeyDriveFileUpdated,
		// When the Stack saw it: the client can set the dates of the file.
		Time: time.Now().UTC(),
	}
}
