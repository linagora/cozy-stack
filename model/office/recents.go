package office

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/recents"
)

// OpenSession records the opening of an OnlyOffice editing session: a view for
// the opener, who is added to the participants of the session key so the
// document server save callbacks credit everyone who opened it. A nil user
// marks an opener that is not a person (anonymous share, service token):
// nothing is recorded, and later callbacks find no participant so they create
// nothing either.
func OpenSession(inst, user *instance.Instance, key string) {
	if user == nil || !recents.Enabled(user) {
		return
	}
	detector, err := GetStore().GetDoc(inst, key)
	if err != nil || detector == nil || detector.ID == "" {
		return
	}
	if err := GetStore().AddParticipant(inst, key, user.Domain); err != nil {
		inst.Logger().WithNamespace("office").
			Warnf("Cannot remember the opener of the office session %s: %s", key, err)
		return
	}
	if err := recents.RecordActivity(user, inst, detector.ID, recents.KindView); err != nil {
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot record the office open of %s: %s", detector.ID, err)
	}
}

// recordSaveActivity credits an OnlyOffice save to every person who opened the
// editing session. The document server does not tell who saved: crediting all
// participants is the closest exact attribution — assumed limit: a participant
// who only viewed is credited with the edit too. No remembered participant (a
// stale callback) means no activity.
func recordSaveActivity(inst *instance.Instance, saved *ConflictDetector) {
	if saved == nil || saved.ID == "" {
		return
	}
	for _, domain := range saved.Participants() {
		user, err := lifecycle.GetInstance(domain)
		if err != nil {
			// The participant's instance is gone or not on this stack: their
			// activity is lost (ADR scope). Log it, but never fail the save.
			inst.Logger().WithNamespace("recents").
				Warnf("Cannot record the office save of %s: instance %s not found", saved.ID, domain)
			continue
		}
		if !recents.Enabled(user) {
			continue
		}
		if err := recents.RecordActivity(user, inst, saved.ID, recents.KindEdit); err != nil {
			inst.Logger().WithNamespace("recents").
				Warnf("Cannot record the office save of %s: %s", saved.ID, err)
		}
	}
}
