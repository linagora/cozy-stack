package files

import (
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// RecordActivity captures a personal activity on a file or a folder for the
// person behind the request. It must be called after the successful write, with
// the item as written, the other one being nil. The row write is best-effort: a
// failed write is a lost activity (accepted for a recents list) and never fails
// the request.
func RecordActivity(c echo.Context, dir *vfs.DirDoc, file *vfs.FileDoc, kind recents.Kind) {
	if actor := ActorInstance(c); actor != nil {
		RecordActivityFor(actor, middlewares.GetInstance(c), dir, file, kind)
	}
}

// RecordActivityFor is RecordActivity for a caller that already knows the
// person: the actor, and the holder of the item. On an item of another
// instance, it chooses the drive of the actor, and records nothing when no drive
// applies.
func RecordActivityFor(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc, kind recents.Kind) {
	var drive *recents.Drive
	if actor.Domain != holder.Domain {
		// recents.RecordActivity checks it too, but only after the drive has
		// been computed for nothing on every write of a shared drive.
		if !recents.Enabled(actor) {
			return
		}
		var err error
		drive, err = sharing.ActivityDrive(actor, holder, dir, file)
		if err != nil {
			holder.Logger().WithNamespace("recents").
				Warnf("Cannot find the drive of an activity of %s: %s", actor.Domain, err)
			return
		}
		if drive == nil {
			return
		}
	}
	if err := recents.RecordActivity(actor, holder, dir, file, kind, drive); err != nil {
		holder.Logger().WithNamespace("recents").
			Warnf("Cannot record an activity of %s: %s", actor.Domain, err)
	}
}

// ActorInstance returns the instance of the person behind the request, as
// resolved by the actor middleware. It returns nil when the request is not a
// person (konnector, service token, other OAuth client, CLI, anonymous share by
// link, background worker), when the member has not accepted the sharing yet, or
// when the member's instance is not on this stack: those record no activity.
func ActorInstance(c echo.Context) *instance.Instance {
	// The actor is resolved along with the permission of the request.
	if _, err := middlewares.GetPermission(c); err != nil {
		return nil
	}
	actor, ok := middlewares.GetActor(c)
	if !ok || !actor.Person {
		return nil
	}
	return actor.Instance
}

// recordEdit records an edit of the person on the item a handler has just
// written, the other one being nil, unless the write is an automatic one.
func recordEdit(c echo.Context, dir *vfs.DirDoc, file *vfs.FileDoc) {
	if !isAutomaticWrite(c, file) {
		RecordActivity(c, dir, file, recents.KindEdit)
	}
}

// backupDeviceIDsKey is the metadata the flagship app sets on the files and
// the device folder of a photo backup.
const backupDeviceIDsKey = "backupDeviceIds"

// isAutomaticWrite tells whether a write is performed by the flagship app on
// its own rather than by the person: the photo backup, the konnector
// destination folders, the konnector traces and the support logs. An upload or
// an edit the person makes from the app (OS share sheet) is a write like any
// other. file is nil for a folder: the flagship app writes folders only for its
// own automatic creations, a folder the person creates or restores goes through
// the Drive webview session.
func isAutomaticWrite(c echo.Context, file *vfs.FileDoc) bool {
	if file != nil {
		if _, ok := file.Metadata[backupDeviceIDsKey]; ok {
			return true
		}
	}
	if actor, ok := middlewares.GetActor(c); !ok || !actor.Flagship {
		return false
	}
	if file == nil {
		return true
	}
	fpath, err := file.Path(middlewares.GetInstance(c).VFS())
	if err != nil {
		return false
	}
	return strings.HasPrefix(fpath, "/Settings/Logs/") ||
		strings.HasPrefix(fpath, "/Settings/AALogs/")
}
