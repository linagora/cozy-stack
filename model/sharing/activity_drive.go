package sharing

import (
	"net/url"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
)

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

	scope, err := NewAccessResolver(holder).DriveForMember(id, actorMember(actor))
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
func actorMember(actor *instance.Instance) *Member {
	// Without an email, the member is matched by its instance only.
	email, _ := actor.SettingsEMail()
	u := url.URL{Scheme: actor.Scheme(), Host: actor.Domain}
	return &Member{Email: email, Instance: u.String()}
}
