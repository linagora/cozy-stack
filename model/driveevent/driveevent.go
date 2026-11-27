// Package driveevent publishes on the activity exchange what happens in the
// personal drive of an instance, one event at a time, for the Twake webhook
// gateway (ADR 075 of linagora/twake-adr). Shared drives report a digest
// instead, see the driveactivity package (ADR 010).
package driveevent

import (
	"bytes"
	"context"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/messaging"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
)

// publisher is where the events are published, nil until SetPublisher is
// called.
var publisher messaging.Publisher

// SetPublisher sets where the events are published.
func SetPublisher(pub messaging.Publisher) {
	publisher = pub
}

// Data is the data of an event: what it is about, and who it is for.
type Data struct {
	Object     Object      `json:"object"`
	Recipients []Recipient `json:"recipients"`
}

// Object is what an event is about: its id, name and folder, never its
// content.
type Object struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Folder    *Reference `json:"folder,omitempty"`
	Shortcut  *Reference `json:"shortcut,omitempty"`
	Container Container  `json:"container"`
}

// Reference is the id of something an event points to.
type Reference struct {
	ID string `json:"id"`
}

// Container is the drive the object is in.
type Container struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Recipient is a person an event is for, by uuid.
type Recipient struct {
	UUID   string `json:"uuid"`
	Reason string `json:"reason"`
}

// FileEventsOn says if the context of the instance publishes an event per
// file of the personal drive. It is off by default, as a sync can create
// thousands of files at once (see ADR 010).
func FileEventsOn(inst *instance.Instance) bool {
	settings, ok := inst.SettingsContext()
	if !ok {
		return false
	}
	cfg, _ := settings["drive_activity"].(map[string]interface{})
	on, _ := cfg["file_events"].(bool)
	return on
}

// FileCreated publishes that a file was created in the personal drive of the
// instance. byOwner tells if the owner created it, rather than someone with a
// sharing link.
func FileCreated(ctx context.Context, inst *instance.Instance, file *vfs.FileDoc, byOwner bool) {
	if file == nil || !FileEventsOn(inst) {
		return
	}
	event := rabbitmq.CloudEvent{
		ID:   file.DocID,
		Type: rabbitmq.RoutingKeyDriveFileCreated,
		// When the Stack saw it: the client can set the dates of the file.
		Time: time.Now().UTC(),
	}
	publish(ctx, inst, event, fileObject(inst, file), byOwner)
}

// FileUpdated publishes that the content of a file of the personal drive was
// replaced, from olddoc to newdoc. byOwner tells if the owner replaced it,
// rather than someone with a sharing link or an editor of an office document.
// The same content again is no update, and the updates of a window are
// published as one at its end.
func FileUpdated(ctx context.Context, inst *instance.Instance, olddoc, newdoc *vfs.FileDoc, byOwner bool) {
	if newdoc == nil || !FileEventsOn(inst) {
		return
	}
	if olddoc != nil && len(olddoc.MD5Sum) > 0 && bytes.Equal(olddoc.MD5Sum, newdoc.MD5Sum) {
		return
	}
	if groupUpdate(inst, newdoc, byOwner) {
		return
	}
	publish(ctx, inst, updatedEvent(newdoc), fileObject(inst, newdoc), byOwner)
}

// FileDeleted publishes that a file of the personal drive was moved to the
// trash. file is the file as it was before, in its folder.
func FileDeleted(ctx context.Context, inst *instance.Instance, file *vfs.FileDoc, byOwner bool) {
	if file == nil || !FileEventsOn(inst) {
		return
	}
	// A file restored then trashed again has another revision.
	event := rabbitmq.CloudEvent{
		ID:   file.DocID + "/deleted/" + file.DocRev,
		Type: rabbitmq.RoutingKeyDriveFileDeleted,
		Time: time.Now().UTC(),
	}
	publish(ctx, inst, event, fileObject(inst, file), byOwner)
}

// ShareReceived publishes that someone shared a file, a folder or a drive with
// the owner of the instance, who has yet to accept it. It is published in
// every context, as people receive few sharings. The person who shares is on
// another instance, whose uuid this one does not know, so the event names no
// actor. shortcutID is the shortcut to the sharing in the drive of the owner,
// which apps without the permission on the sharings can read.
func ShareReceived(ctx context.Context, inst *instance.Instance, sharingID, title, shortcutID string) {
	event := rabbitmq.CloudEvent{
		ID:   sharingID + "/received",
		Type: rabbitmq.RoutingKeyDriveShareReceived,
		Time: time.Now().UTC(),
	}
	object := Object{
		Type:      "sharing",
		ID:        sharingID,
		Title:     title,
		Container: Container{Kind: "drive", ID: inst.Domain},
	}
	if shortcutID != "" {
		object.Shortcut = &Reference{ID: shortcutID}
	}
	publish(ctx, inst, event, object, false)
}

func fileObject(inst *instance.Instance, file *vfs.FileDoc) Object {
	return Object{
		Type:      "file",
		ID:        file.DocID,
		Title:     file.DocName,
		Folder:    &Reference{ID: file.DirID},
		Container: Container{Kind: "drive", ID: inst.Domain},
	}
}

// publish sends the event to the owner of the instance. The change is already
// made, so the event is best effort: publish only logs its errors.
func publish(ctx context.Context, inst *instance.Instance, event rabbitmq.CloudEvent, object Object, byOwner bool) {
	if err := publishWith(ctx, publisher, inst, event, object, byOwner); err != nil {
		inst.Logger().WithNamespace("drive-event").
			Warnf("Cannot publish %s for %s: %s", event.Type, object.ID, err)
	}
}

// publishWith sends the event with pub, and returns why it could not.
func publishWith(ctx context.Context, pub messaging.Publisher, inst *instance.Instance, event rabbitmq.CloudEvent, object Object, byOwner bool) error {
	if pub == nil {
		return nil
	}
	// The events name people by uuid only: without it, no one could get it.
	if inst.OIDCUUID == "" {
		inst.Logger().WithNamespace("drive-event").
			Debugf("No %s event: the uuid of the owner is unknown until they log in", event.Type)
		return nil
	}
	event.TwakeOrg = inst.OrgID
	event.Data = Data{
		Object:     object,
		Recipients: []Recipient{{UUID: inst.OIDCUUID, Reason: "owner"}},
	}
	if byOwner {
		event.TwakeActorID = inst.OIDCUUID
	}
	return pub.Publish(ctx, rabbitmq.ActivityRequest(inst.ContextName, event))
}
