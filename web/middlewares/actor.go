package middlewares

import (
	"errors"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/labstack/echo/v4"
)

const contextRequestActor = "request_actor"

// Actor describes the authenticated actor associated with the current request.
type Actor struct {
	Kind        string
	DisplayName string
	Domain      string
	// Instance is the actor's instance when it lives on this stack: the
	// request instance for its owner's tokens, the instance of a sharing
	// member who has accepted the sharing. It is nil for an anonymous actor,
	// a member on another stack, or a member who has not accepted yet (the
	// Cozy URL then comes from the sharer's contacts).
	Instance *instance.Instance
	// Person tells that the original token of the request stands for a
	// person: a session-bound app token, the flagship app, an OAuth client of
	// the desktop kind, a sharing member's share-interact code, or their
	// share-preview code once they have accepted the sharing. It is computed
	// when the actor is first resolved, so it is kept when the shared drive
	// proxy swaps the token of the request.
	Person bool
	// Flagship tells that the original token of the request is the flagship
	// app's: a certified OAuth client, or the "*" scope granted to the flagship
	// app when its context skips the certification. Like Person, it is computed
	// when the actor is first resolved, so it survives the token swap of the
	// shared drive proxy: a consumer must read it rather than inspect the token
	// of the request, which the proxy has replaced with the sharing key. It is
	// false for every other actor.
	Flagship bool
}

// GetActor returns the actor associated with the request context.
func GetActor(c echo.Context) (*Actor, bool) {
	v := c.Get(contextRequestActor)
	if v != nil {
		actor, ok := v.(*Actor)
		return actor, ok && actor != nil
	}
	return nil, false
}

// SetActor stores the current request actor in the context.
func SetActor(c echo.Context, actor *Actor) {
	c.Set(contextRequestActor, actor)
}

// ResolveRequestActor derives the request actor from the resolved permission.
// It must follow the ParseJWT call that gave this permission: a share code
// gets its member from what ParseJWT kept in the context.
func ResolveRequestActor(c echo.Context, inst *instance.Instance, pdoc *permission.Permission) error {
	if pdoc == nil {
		SetActor(c, nil)
		return nil
	}

	switch pdoc.Type {
	case permission.TypeOauth, permission.TypeCLI, permission.TypeWebapp, permission.TypeKonnector:
		actor := actorFromInstance(inst)
		actor.Person = isPersonToken(c, pdoc)
		actor.Flagship = isFlagshipToken(c, pdoc)
		SetActor(c, actor)
		return nil

	case permission.TypeShareInteract:
		if _, ok := GetActor(c); ok {
			return nil
		}

		SetActor(c, actorFromMember(inst, getShareMember(c)))
		return nil

	case permission.TypeSharePreview:
		member := getShareMember(c)
		// A preview code stands for its member only once they have accepted
		// the sharing.
		if member == nil || member.Status != sharing.MemberStatusReady {
			SetActor(c, anonymousShareActor())
			return nil
		}

		SetActor(c, actorFromMember(inst, member))
		return nil

	case permission.TypeShareByLink:
		SetActor(c, anonymousShareActor())
		return nil

	case permission.TypeRegister:
		SetActor(c, nil)
		return nil

	default:
		SetActor(c, nil)
		return nil
	}
}

func actorFromInstance(inst *instance.Instance) *Actor {
	displayName, err := inst.SettingsPublicName()
	if err != nil || displayName == "" {
		displayName = inst.Domain
	}
	return &Actor{
		Kind:        vfs.TrashedByKindMember,
		DisplayName: displayName,
		Domain:      inst.Domain,
		Instance:    inst,
	}
}

// isPersonToken tells if a token of the instance owner stands for the user,
// and not for a konnector, a service or the CLI.
func isPersonToken(c echo.Context, pdoc *permission.Permission) bool {
	switch pdoc.Type {
	case permission.TypeWebapp:
		// A session-bound app token is the user in their browser; an app token
		// with no session is a service token.
		claims, ok := c.Get("claims").(permission.Claims)
		return ok && claims.SessionID != ""

	case permission.TypeOauth:
		// The flagship app and cozy-desktop act for their user. The desktop kind
		// is declared by the client itself: any client declaring it counts.
		if client, ok := pdoc.Client.(*oauth.Client); ok &&
			(client.Flagship || client.ClientKind == "desktop") {
			return true
		}
		// Only the flagship app asks for the "*" scope: it is not certified
		// when its context skips the certification, where the stack grants
		// that scope to any OAuth client sending a PKCE challenge.
		claims, ok := c.Get("claims").(permission.Claims)
		return ok && claims.Scope == "*"

	default:
		return false
	}
}

// isFlagshipToken tells if a token is the flagship app's: a certified OAuth
// client, or the "*" scope granted to any OAuth client sending a PKCE
// challenge in a context that skips the certification. It is a property of the
// token alone, so it is the same for every request of the flagship app. It is
// the subset of isPersonToken that excludes cozy-desktop and session tokens.
func isFlagshipToken(c echo.Context, pdoc *permission.Permission) bool {
	if pdoc.Type != permission.TypeOauth {
		return false
	}
	if client, ok := pdoc.Client.(*oauth.Client); ok && client.Flagship {
		return true
	}
	claims, ok := c.Get("claims").(permission.Claims)
	return ok && claims.Scope == "*"
}

func actorFromMember(inst *instance.Instance, member *sharing.Member) *Actor {
	if member == nil {
		return nil
	}
	displayName := member.PublicName
	if displayName == "" {
		displayName = member.PrimaryName()
	}
	actor := &Actor{
		Kind:        vfs.TrashedByKindMember,
		DisplayName: displayName,
		Domain:      member.InstanceHost(),
		Person:      true,
	}
	if actor.Domain != "" {
		if local := memberInstance(inst, actor.Domain); local != nil {
			actor.Domain = local.Domain
			if member.Status == sharing.MemberStatusReady {
				actor.Instance = local
			}
		}
	}
	return actor
}

// memberInstance returns the instance of a sharing member when it lives on
// this stack, or nil. A member on another stack is expected and silent.
func memberInstance(inst *instance.Instance, domain string) *instance.Instance {
	found, err := lifecycle.GetInstance(domain)
	if err == nil {
		return found
	}
	if !errors.Is(err, instance.ErrNotFound) && !errors.Is(err, instance.ErrIllegalDomain) {
		inst.Logger().WithNamespace("actor").
			Warnf("Cannot load the instance of member %s: %s", domain, err)
	}
	return nil
}

func anonymousShareActor() *Actor {
	return &Actor{Kind: vfs.TrashedByKindAnonymousShare}
}
