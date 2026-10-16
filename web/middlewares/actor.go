package middlewares

import (
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
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
	case permission.TypeOauth, permission.TypeCLI, permission.TypeWebapp, permission.TypeKonnector, permission.TypeIntent:
		SetActor(c, actorFromInstance(inst))
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

		SetActor(c, actorFromMember(member))
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
	}
}

func actorFromMember(member *sharing.Member) *Actor {
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
	}
	if actor.Domain != "" {
		if inst, err := lifecycle.GetInstance(actor.Domain); err == nil {
			actor.Domain = inst.Domain
		}
	}
	return actor
}

func anonymousShareActor() *Actor {
	return &Actor{Kind: vfs.TrashedByKindAnonymousShare}
}
