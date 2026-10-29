package files

import (
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// RecordActivity captures a personal activity on a file for the person behind
// the request. The row write is best-effort: a failed write is a lost activity
// (accepted for a recents list) and never fails the request.
func RecordActivity(c echo.Context, fileID string, kind recents.Kind) {
	user := ActorInstance(c)
	if user == nil || !recents.Enabled(user) {
		return
	}
	inst := middlewares.GetInstance(c)
	if err := recents.RecordActivity(user, inst, fileID, kind); err != nil {
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot record %s activity on %s: %s", kind, fileID, err)
	}
}

// ActorInstance returns the instance of the person behind the request, as
// resolved by the actor middleware: the instance owner for a session-bound app
// token, the flagship app (certified, or with the "*" scope) or a desktop
// OAuth client, and the sharing member for a share-interact code, once the
// member has accepted the sharing. On the shared drive proxy, the person is
// checked on the original token of the recipient when the owner's instance is
// served directly on this stack; a request forwarded over HTTP only carries
// the drive token and counts as the recipient (ADR). It returns nil when the
// request is not a person (konnector, service token, other OAuth client, CLI,
// anonymous share by link, background worker), when the member has not
// accepted the sharing yet, or when the member's instance is not on this
// stack: those record no activity.
func ActorInstance(c echo.Context) *instance.Instance {
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
