package banner

import (
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// stackAuthor marks the documents clients are allowed to trust. Permissions
// scope by doctype and verb, never by field, so any application able to record
// a dismissal can also author a document that looks like a platform message.
const stackAuthor = "stack"

// docID is the one document a category can have. Fixing the id makes CouchDB
// refuse a second concurrent create, which is the only layer that sees every
// stack process; a lookup-then-create with a generated id let two evaluations
// racing each other write two banners the client then rendered side by side.
func docID(category string) string { return "banner-" + category }

// Merge carries a client written dismissal forward: re-materializing the same
// occurrence must not resurrect a banner the user has already closed, and only
// a new BannerID clears it. StartsAt is carried the same way, so it stays the
// moment the occurrence began rather than the last re-evaluation.
func Merge(fresh, stored *Banner) *Banner {
	merged := fresh.clone()
	if stored == nil {
		return merged
	}
	merged.DocID = stored.DocID
	merged.DocRev = stored.DocRev
	if stored.BannerID == fresh.BannerID {
		merged.DismissedAt = stored.DismissedAt
		if stored.StartsAt != nil {
			at := *stored.StartsAt
			merged.StartsAt = &at
		}
	}
	if stored.Metadata != nil {
		merged.Metadata = stored.Metadata.Clone()
	}
	return merged
}

// Materialize writes the banner a rule produced, or deletes the stored one
// when the rule no longer produces any. It is idempotent: running it twice for
// the same state leaves the same document.
func Materialize(db prefixer.Prefixer, category string, fresh *Banner, now time.Time) error {
	stored, err := Stored(db, category)
	if err != nil {
		return err
	}

	if fresh == nil {
		if stored == nil {
			return nil
		}
		log(db).Infof("%s: condition cleared, removing %s", category, stored.BannerID)
		return couchdb.DeleteDoc(db, stored)
	}

	merged := Merge(fresh, stored)
	merged.Category = category
	ensureEscapable(merged)
	stamp(merged, now)

	if stored == nil {
		merged.DocID = docID(category)
		log(db).Infof("%s: creating %s (%s)", category, merged.BannerID, merged.Severity)
		return couchdb.CreateNamedDocWithDB(db, merged)
	}
	if !changed(merged, stored) {
		log(db).Debugf("%s: %s unchanged", category, merged.BannerID)
		return nil
	}
	log(db).Infof("%s: updating %s to %s (%s)", category, stored.BannerID, merged.BannerID, merged.Severity)
	return couchdb.UpdateDocWithOld(db, merged, stored)
}

// ensureEscapable keeps a blocking dialog from covering the application with
// nothing to click and no way to close it, where a reload is the only way out
// and brings the same banner straight back. Only a producer mistake gets
// there, and the message is still worth showing, so it becomes closable rather
// than being dropped.
func ensureEscapable(b *Banner) {
	if b.Surface == SurfaceModal && b.CTA == nil && !b.Dismissible {
		b.Dismissible = true
	}
}

func log(db prefixer.Prefixer) *logger.Entry {
	return logger.WithDomain(db.DomainName()).WithNamespace("banner")
}

func stamp(b *Banner, now time.Time) {
	if b.Metadata == nil {
		b.Metadata = metadata.New()
	}
	// The stored metadata may come from a document an application authored,
	// so the envelope is repaired rather than trusted.
	if b.Metadata.MetadataVersion == 0 {
		b.Metadata.MetadataVersion = metadata.MetadataVersion
	}
	if b.Metadata.CreatedAt.IsZero() {
		b.Metadata.CreatedAt = now
	}
	b.Metadata.CreatedByApp = stackAuthor
	b.Metadata.DocTypeVersion = DocTypeVersion
	b.Metadata.UpdatedAt = now
}

// changed keeps an unchanged evaluation from bumping the revision on every
// trigger, which would wake every realtime client for nothing. DocID, DocRev
// and DismissedAt are excluded because Merge takes them from the stored
// document, and Source.At because it moves on every evaluation by design.
func changed(fresh, stored *Banner) bool {
	return fresh.BannerID != stored.BannerID ||
		fresh.Category != stored.Category ||
		fresh.Severity != stored.Severity ||
		fresh.Surface != stored.Surface ||
		fresh.Title != stored.Title ||
		fresh.Text != stored.Text ||
		fresh.Lang != stored.Lang ||
		fresh.Priority != stored.Priority ||
		fresh.Dismissible != stored.Dismissible ||
		timeChanged(fresh.StartsAt, stored.StartsAt) ||
		timeChanged(fresh.EndsAt, stored.EndsAt) ||
		ctaChanged(fresh.CTA, stored.CTA) ||
		ctaChanged(fresh.SecondaryCTA, stored.SecondaryCTA) ||
		stampChanged(fresh.Metadata, stored.Metadata)
}

// stampChanged stops a document an application authored from keeping its own
// createdByApp just because its wording matches the evaluation; a client drops
// such a document as untrusted.
func stampChanged(fresh, stored *metadata.CozyMetadata) bool {
	if fresh == nil || stored == nil {
		return (fresh == nil) != (stored == nil)
	}
	return fresh.CreatedByApp != stored.CreatedByApp ||
		fresh.DocTypeVersion != stored.DocTypeVersion ||
		fresh.MetadataVersion != stored.MetadataVersion
}

func timeChanged(fresh, stored *time.Time) bool {
	if fresh == nil || stored == nil {
		return (fresh == nil) != (stored == nil)
	}
	return !fresh.Equal(*stored)
}

func ctaChanged(fresh, stored *CTA) bool {
	if fresh == nil || stored == nil {
		return (fresh == nil) != (stored == nil)
	}
	return *fresh != *stored
}

// Stored returns the banner materialized for a category, or nil when there is
// none. A missing database is the first call on a fresh instance.
func Stored(db prefixer.Prefixer, category string) (*Banner, error) {
	var doc Banner
	err := couchdb.GetDoc(db, consts.Banners, docID(category), &doc)
	if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// refresh re-evaluates one category of an instance from a bus event, and is
// what both the payment and the trial entry points are. eventAt is the moment
// Stripe recorded the event, not the moment this runs, so it both stamps the
// document and orders it against what is stored. evaluate receives the manager
// premium page, empty when the stack does not know it.
func refresh(domain, category string, eventAt time.Time, evaluate func(inst *instance.Instance, managerURL string) *Banner) error {
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}
	if !inst.HasBannersEnabled() {
		return nil
	}

	// One lock name for every category, deliberately: at expiry the Cloudery
	// sends trial.changed and payment.failed from one Stripe event, and this
	// makes the two run one after the other rather than racing.
	mu := config.Lock().ReadWrite(inst, "banners")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	stale, err := supersededBy(inst, category, eventAt)
	if err != nil || stale {
		return err
	}

	var managerURL string
	if premium, err := inst.ManagerURL(instance.ManagerPremiumURL); err == nil {
		managerURL = premium
	}

	return Materialize(inst, category, evaluate(inst, managerURL), time.Now())
}

// supersededBy reports whether the stored banner was produced by an event at
// least as recent as this one, in which case this one is a redelivery or
// arrived out of order. Bus delivery is at-least-once and unordered, so
// without this a redelivered failure could overwrite a recovery.
//
// ponytail: Source.At is the event that last changed the document, not the
// last one seen, since an unchanged re-evaluation writes nothing. So an event
// redelivered after a later one deleted the document recreates it. Both rules
// bound the damage themselves, the trial through its validity window and the
// payment one through the next event, and both need a single queue reordered
// or a replay past the Cloudery's own dedupe. Record the last applied event
// time per instance if that ever happens.
func supersededBy(db prefixer.Prefixer, category string, eventAt time.Time) (bool, error) {
	stored, err := Stored(db, category)
	if err != nil || stored == nil {
		return false, err
	}
	return !stored.Source.At.Before(eventAt), nil
}
