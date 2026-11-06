package files

import (
	"net/url"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
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
		drive, err = ActivityDrive(actor, holder, dir, file)
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

// ActivityDrive computes, on the holder of an item, the drive that a row of the
// actor on this item must hold, from the position of the item and never from the
// route or the code of the request. It returns nil when no drive applies: an
// item outside of the drives of the actor, in a classic sharing for example. It
// returns an error when the item is not under the root of the drive, because it
// has moved in the meantime: the full path on the holder must not be used
// instead.
func ActivityDrive(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc) (*recents.Drive, error) {
	var id, fullpath string
	var err error
	if dir != nil {
		id, fullpath = dir.ID(), dir.Fullpath
	} else {
		id = file.ID()
		fullpath, err = file.Path(holder.VFS())
		if err != nil {
			return nil, err
		}
	}

	scope, err := sharing.NewAccessResolver(holder).DriveForMember(id, actorMember(actor))
	if err != nil || scope == nil {
		return nil, err
	}
	path, err := scope.PathFromRoot(fullpath)
	if err != nil {
		return nil, err
	}
	return &recents.Drive{ID: scope.Sharing.SID, Path: path}, nil
}

// actorMember returns the reference of an actor to match the members of the
// sharings of another instance (see sharing.Sharing.MemberMatching). The request
// knows no member when the actor comes from the original token of a recipient on
// the shared drive proxy, and only the member of its own sharing otherwise: the
// instance of the actor gives the same reference in both cases.
func actorMember(actor *instance.Instance) *sharing.Member {
	// Without an email, the member is matched by its instance only.
	email, _ := actor.SettingsEMail()
	u := url.URL{Scheme: actor.Scheme(), Host: actor.Domain}
	return &sharing.Member{Email: email, Instance: u.String()}
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

// backupDeviceIDsKey is the metadata the flagship app sets on the files and
// the device folder of a photo backup.
const backupDeviceIDsKey = "backupDeviceIds"

// isAutomaticCreation tells whether a creation is written by the flagship app
// on its own rather than by the person: the photo backup, the konnector
// destination folders, the konnector traces and the support logs. An upload
// the person makes from the app (OS share sheet) is a file creation like any
// other. created is nil for a folder: the flagship app creates folders only
// for its own automatic writes, a folder the person creates goes through the
// Drive webview session.
func isAutomaticCreation(c echo.Context, created *vfs.FileDoc) bool {
	if created != nil {
		if _, ok := created.Metadata[backupDeviceIDsKey]; ok {
			return true
		}
	}
	if !isFlagshipClient(c) {
		return false
	}
	if created == nil {
		return true
	}
	fpath, err := created.Path(middlewares.GetInstance(c).VFS())
	if err != nil {
		return false
	}
	return strings.HasPrefix(fpath, "/Settings/Logs/") ||
		strings.HasPrefix(fpath, "/Settings/AALogs/")
}

// isFlagshipClient tells whether the request is made with an OAuth token of
// the flagship app: a certified client, or the "*" scope given to the flagship
// app when its context skips the certification.
func isFlagshipClient(c echo.Context) bool {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil || pdoc == nil || pdoc.Type != permission.TypeOauth {
		return false
	}
	if client, ok := pdoc.Client.(*oauth.Client); ok && client.Flagship {
		return true
	}
	claims, ok := c.Get("claims").(permission.Claims)
	return ok && claims.Scope == "*"
}
