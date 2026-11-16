package office

import (
	"errors"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
)

// OpenSession records the opening of an OnlyOffice editing session: a view for
// the opener, who is added to the participants of the session so the document
// server save callbacks credit everyone who opened it. A nil user marks an
// opener that is not a person (anonymous share, service token): nothing is
// recorded, and later callbacks find no participant so they create nothing
// either. inDrive tells that a shared drive gives the document to an opener
// who does not own it.
func OpenSession(inst, user *instance.Instance, file *vfs.FileDoc, inDrive bool, key string) {
	if user == nil || file == nil || !recents.Enabled(user) {
		return
	}
	if err := GetStore().AddParticipant(inst, key, user.Domain, inDrive); err != nil {
		inst.Logger().WithNamespace("office").
			Warnf("Cannot remember the opener of the office session %s: %s", key, err)
		return
	}
	if err := recents.RecordActivity(user, inst, nil, file, recents.KindView, inDrive); err != nil {
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot record the office open of %s: %s", file.ID(), err)
	}
}

// sessionParticipants returns the people who opened an editing session, read
// at the time of the save callback: an opening during the download of the
// document is credited too. An error is logged and gives no participant.
func sessionParticipants(inst *instance.Instance, key string) []Participant {
	participants, err := GetStore().GetParticipants(inst, key)
	if err != nil {
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot read the participants of the office session %s: %s", key, err)
	}
	return participants
}

// recordSaveActivity credits an OnlyOffice save to every person who opened the
// editing session. The document server does not tell who saved: crediting all
// participants is the closest exact attribution — assumed limit: a participant
// who only viewed is credited with the edit too. No remembered participant (a
// stale callback) means no activity.
func recordSaveActivity(inst *instance.Instance, fileID string, participants []Participant) {
	if fileID == "" || len(participants) == 0 {
		return
	}
	file, err := inst.VFS().FileByID(fileID)
	if err != nil {
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot record the office save of %s: %s", fileID, err)
		return
	}
	for _, p := range participants {
		user, err := lifecycle.GetInstance(p.Domain)
		if err != nil {
			// A participant whose instance is gone or not on this stack is
			// expected: their activity is silently lost (ADR scope). Any other
			// error is logged, but never fails the save.
			if !errors.Is(err, instance.ErrNotFound) && !errors.Is(err, instance.ErrIllegalDomain) {
				inst.Logger().WithNamespace("recents").
					Warnf("Cannot record the office save of %s: cannot load the instance %s: %s", fileID, p.Domain, err)
			}
			continue
		}
		if err := recents.RecordActivity(user, inst, nil, file, recents.KindEdit, p.InDrive); err != nil {
			inst.Logger().WithNamespace("recents").
				Warnf("Cannot record the office save of %s: %s", fileID, err)
		}
	}
}
