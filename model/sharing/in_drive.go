package sharing

import (
	"net/url"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
)

// InDrive tells, on the holder of an item, whether a drive gives this item to
// the actor, from the position of the item and never from the route or the
// code of the request. It is false for an item outside of the drives of the
// actor, in a classic sharing for example.
func InDrive(actor, holder *instance.Instance, dir *vfs.DirDoc, file *vfs.FileDoc) (bool, error) {
	var id string
	if dir != nil {
		id = dir.ID()
	} else {
		id = file.ID()
	}
	scope, err := NewAccessResolver(holder).DriveForMember(id, actorMember(actor))
	if err != nil {
		return false, err
	}
	return scope != nil, nil
}

// actorMember returns the reference of an actor to match the members of the
// sharings of another instance (see sharing.Sharing.MemberMatching). The request
// knows no member when the actor comes from the original token of a recipient on
// the shared drive proxy, and only the member of its own sharing otherwise: the
// instance of the actor gives the same reference in both cases.
func actorMember(actor *instance.Instance) *Member {
	// Without an email, the member is matched by its instance only.
	email, _ := actor.SettingsEMail()
	u := url.URL{Scheme: actor.Scheme(), Host: actor.Domain}
	return &Member{Email: email, Instance: u.String()}
}
