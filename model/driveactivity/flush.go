package driveactivity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/messaging"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/gofrs/uuid/v5"
)

// WorkerFlush publishes the counts of a drive at the end of a window.
const WorkerFlush = "drive-activity"

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   WorkerFlush,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 5,
		Reserved:     true,
		Timeout:      5 * time.Minute,
		WorkerFunc:   workFlush,
	})
}

// FlushMessage is the message of the drive-activity jobs.
type FlushMessage struct {
	SharingID string `json:"sharing_id"`
	Window    string `json:"window"`
}

// FolderChangedData is the data of com.twake.drive.folder.changed.v1.
type FolderChangedData struct {
	Object  Object   `json:"object"`
	Files   Files    `json:"files"`
	Samples []string `json:"samples"`
}

// Object is the top folder the files changed in.
type Object struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Container Container `json:"container"`
}

// Container is the drive holding the folder.
type Container struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Files counts the files changed in the folder during the window.
type Files struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Deleted int `json:"deleted"`
}

func workFlush(ctx *job.TaskContext) error {
	var msg FlushMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	return flush(ctx, ctx.Publisher, ctx.Instance, &msg)
}

// flush publishes the counts taken for the window, and only then drops them,
// so that a retry publishes the same events.
func flush(ctx context.Context, pub messaging.Publisher, inst *instance.Instance, msg *FlushMessage) error {
	st := getStore()
	drive := driveKey(inst, msg.SharingID)
	counts, err := st.take(drive, msg.Window)
	if err != nil {
		return err
	}
	if len(counts) > 0 {
		s, err := sharing.FindSharing(inst, msg.SharingID)
		if err != nil && !couchdb.IsNotFoundError(err) {
			return err
		}
		if err == nil && s.Active && s.ReportsActivity() {
			if err := publish(ctx, pub, inst, s, msg.Window, counts); err != nil {
				return err
			}
		}
	}
	return st.drop(drive, msg.Window)
}

type digest struct {
	member string
	top    *vfs.DirDoc
	tally
}

func publish(ctx context.Context, pub messaging.Publisher, inst *instance.Instance, s *sharing.Sharing, window string, counts map[key]*tally) error {
	rootID, err := s.DriveRootID()
	if err != nil {
		inst.Logger().WithNamespace("drive-activity").
			Warnf("Cannot publish the activity of drive %s: %s", s.SID, err)
		return nil
	}
	digests, err := group(inst.VFS(), rootID, counts)
	if err != nil {
		return err
	}
	for _, d := range digests {
		samples := d.samples
		if samples == nil {
			samples = []string{}
		}
		id := uuid.NewV5(uuid.NamespaceURL, strings.Join([]string{"twake://drive", s.SID, d.member, d.top.ID(), window}, "/"))
		req := rabbitmq.ActivityRequest(inst.ContextName, rabbitmq.CloudEvent{
			ID:         id.String(),
			Type:       rabbitmq.RoutingKeyDriveFolderChanged,
			Time:       time.Now().UTC(),
			TwakeOrg:   inst.OrgID,
			TwakeActor: d.member,
			Data: FolderChangedData{
				Object: Object{
					Type:      "folder",
					ID:        d.top.ID(),
					Title:     d.top.DocName,
					Container: Container{Kind: "drive", ID: s.SID},
				},
				Files:   Files{Created: d.created, Updated: d.updated, Deleted: d.deleted},
				Samples: samples,
			},
		})
		if err := pub.Publish(ctx, req); err != nil {
			return fmt.Errorf("drive %s: publish the activity of %s: %w", s.SID, d.member, err)
		}
	}
	return nil
}

// group sums the counts of each member under the top folders, the folders
// directly under the root of the drive. A folder that left the drive since
// the change is not counted.
func group(fs vfs.VFS, rootID string, counts map[key]*tally) ([]*digest, error) {
	root, err := fs.DirByID(rootID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	tops := map[string]*vfs.DirDoc{rootID: root}
	topOf := func(folderID string) (*vfs.DirDoc, error) {
		if top, ok := tops[folderID]; ok {
			return top, nil
		}
		dir, err := fs.DirByID(folderID)
		if errors.Is(err, os.ErrNotExist) {
			tops[folderID] = nil
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		rel, ok := strings.CutPrefix(dir.Fullpath, root.Fullpath+"/")
		if !ok {
			tops[folderID] = nil
			return nil, nil
		}
		name, _, _ := strings.Cut(rel, "/")
		top := dir
		if topPath := root.Fullpath + "/" + name; topPath != dir.Fullpath {
			top, err = fs.DirByPath(topPath)
			if err != nil {
				return nil, err
			}
		}
		tops[folderID] = top
		return top, nil
	}

	var digests []*digest
	byTop := make(map[key]*digest)
	for k, t := range counts {
		top, err := topOf(k.folder)
		if err != nil {
			return nil, err
		}
		if top == nil {
			continue
		}
		dk := key{member: k.member, folder: top.ID()}
		d := byTop[dk]
		if d == nil {
			d = &digest{member: k.member, top: top}
			byTop[dk] = d
			digests = append(digests, d)
		}
		d.created += t.created
		d.updated += t.updated
		d.deleted += t.deleted
		for _, name := range t.samples {
			d.sample(name)
		}
	}
	return digests, nil
}
