package files

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
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

// ActorInstance returns the instance of the person behind the request: the
// instance owner for a session-bound app token or a desktop/flagship OAuth
// client, the sharing member for a share-interact token. It returns nil when
// the request is not a person (konnector, service token, CLI, anonymous share,
// background worker): those record no activity. Unsupported write paths (ADR),
// never any activity: openings of mobile OAuth apps, other app tokens and
// konnectors.
func ActorInstance(c echo.Context) *instance.Instance {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil || pdoc == nil {
		return nil
	}
	inst := middlewares.GetInstance(c)

	switch pdoc.Type {
	case permission.TypeShareInteract:
		actor, ok := middlewares.GetActor(c)
		if !ok || actor == nil || actor.Domain == "" {
			return nil
		}
		return memberInstance(inst, actor.Domain)

	case permission.TypeWebapp:
		// A session-bound app token is the user in their browser; an app token
		// with no session is a service token, not a person.
		if claims, ok := c.Get("claims").(permission.Claims); ok && claims.SessionID != "" {
			return inst
		}
		return nil

	case permission.TypeOauth:
		// The flagship app and cozy-desktop act for their user.
		client, ok := pdoc.Client.(*oauth.Client)
		if ok && (client.Flagship || client.ClientKind == "desktop") {
			return inst
		}
		return nil

	default:
		return nil
	}
}

// memberInstance returns the instance of a sharing member by its domain.
func memberInstance(inst *instance.Instance, domain string) *instance.Instance {
	user, err := lifecycle.GetInstance(domain)
	if err != nil {
		// The member's instance is not on this stack: the activity is lost
		// (ADR scope). Log it, but never fail the request.
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot record activity of %s: instance not found", domain)
		return nil
	}
	return user
}
