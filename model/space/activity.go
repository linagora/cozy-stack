package space

import (
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/notification"
	"github.com/cozy/cozy-stack/model/notification/center"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
)

func init() {
	center.RegisterEventMapper(center.NotificationDriveFileCreated, fileCreatedEvent)
}

// ActivityData is the data of an action on a space drive, read by the
// TwakeSpace activity feed. It names the drive, never the space.
type ActivityData struct {
	Object Object `json:"object"`
}

// Object is the file an activity is about.
type Object struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Container Container `json:"container"`
}

// Container is the drive holding an Object.
type Container struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// NotifyFileCreated pushes the drive-file-created notification of a file that
// actor created in the shared drive s, when s is a space drive. A nil actor is
// the organization instance itself.
func NotifyFileCreated(inst *instance.Instance, s *sharing.Sharing, actor *sharing.Member, doc *vfs.FileDoc) error {
	if !s.OrgDrive {
		return nil
	}
	rec, err := recordBySharing(inst, s.SID)
	if err != nil || rec == nil {
		return err
	}
	// Without a member, the request is not limited to the drive: the
	// organization instance can create the file anywhere.
	if in, err := inDrive(inst, s, doc); err != nil || !in {
		return err
	}
	// TwakeSpace checks the actor against the space members, and the address
	// of the organization instance is not one of them.
	var actorEmail string
	if actor != nil {
		actorEmail = actor.Email
	}
	n := &notification.Notification{
		Title: doc.DocName,
		Slug:  consts.DriveSlug,
		Data: map[string]interface{}{
			"OrganizationID": rec.OrganizationID,
			"SharingID":      s.SID,
			"FileID":         doc.ID(),
			"FileName":       doc.DocName,
			"ActorEmail":     actorEmail,
		},
	}
	return center.PushStack(inst.DomainName(), center.NotificationDriveFileCreated, n)
}

func fileCreatedEvent(_ *instance.Instance, n *notification.Notification) (*rabbitmq.PublishRequest, error) {
	sharingID, _ := n.Data["SharingID"].(string)
	orgID, _ := n.Data["OrganizationID"].(string)
	if sharingID == "" || orgID == "" {
		return nil, nil
	}

	fileID, _ := n.Data["FileID"].(string)
	name, _ := n.Data["FileName"].(string)
	actor, _ := n.Data["ActorEmail"].(string)
	return activityRequest(rabbitmq.RoutingKeyDriveFileCreated, orgID, actor, ActivityData{Object: Object{
		Type:      "file",
		ID:        fileID,
		Title:     name,
		Container: Container{Kind: "drive", ID: sharingID},
	}})
}

func inDrive(inst *instance.Instance, s *sharing.Sharing, doc *vfs.FileDoc) (bool, error) {
	rootID, err := s.DriveRootID()
	if err != nil {
		return false, err
	}
	if doc.DirID == rootID {
		return true, nil
	}
	fs := inst.VFS()
	root, err := fs.DirByID(rootID)
	if err != nil {
		return false, err
	}
	parent, err := fs.DirByID(doc.DirID)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(parent.Fullpath, root.Fullpath+"/"), nil
}

func recordBySharing(inst *instance.Instance, sharingID string) (*Record, error) {
	var recs []*Record
	req := &couchdb.FindRequest{
		Selector: mango.Equal("sharing_id", sharingID),
		Limit:    1,
	}
	err := couchdb.FindDocsUnoptimized(inst, consts.Spaces, req, &recs)
	if couchdb.IsNoDatabaseError(err) {
		return nil, nil
	}
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return recs[0], nil
}
