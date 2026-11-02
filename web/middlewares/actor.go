package middlewares

import (
	"errors"
	"strings"

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
	// person: a session-bound app token, the flagship app, cozy-desktop or a
	// sharing member's share-interact code. It is computed when the actor is
	// first resolved, so it is kept when the shared drive proxy swaps the
	// token of the request.
	Person bool
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
func ResolveRequestActor(c echo.Context, inst *instance.Instance, pdoc *permission.Permission) error {
	if pdoc == nil {
		SetActor(c, nil)
		return nil
	}

	switch pdoc.Type {
	case permission.TypeOauth, permission.TypeCLI, permission.TypeWebapp, permission.TypeKonnector:
		actor := actorFromInstance(inst)
		actor.Person = isPersonToken(c, pdoc)
		SetActor(c, actor)
		return nil

	case permission.TypeShareInteract:
		if _, ok := GetActor(c); ok {
			return nil
		}

		token := GetRequestToken(c)
		if token == "" {
			SetActor(c, nil)
			return nil
		}

		token, err := TransformShortcodeToJWT(inst, token)
		if err != nil {
			return err
		}

		sharingID := pdoc.SourceID
		if _, id, ok := strings.Cut(sharingID, "/"); ok {
			sharingID = id
		}

		sharingDoc, err := sharing.FindSharing(inst, sharingID)
		if err != nil {
			return err
		}

		member, err := sharingDoc.FindMemberByInteractCode(inst, token)
		if err != nil {
			return err
		}

		SetActor(c, actorFromMember(inst, member))
		return nil

	case permission.TypeSharePreview, permission.TypeShareByLink:
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
		// The flagship app and cozy-desktop act for their user.
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
		if local := MemberInstance(inst, actor.Domain); local != nil {
			actor.Domain = local.Domain
			if member.Status == sharing.MemberStatusReady {
				actor.Instance = local
			}
		}
	}
	return actor
}

// MemberInstance returns the instance of a sharing member when it lives on
// this stack, or nil. A member on another stack is expected and silent.
func MemberInstance(inst *instance.Instance, domain string) *instance.Instance {
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
