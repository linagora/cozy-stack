package orgdirectory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
)

const maxReconcileAge = 7 * 24 * time.Hour

const commonContactsSeenAtKey = "twakeCommonContactsSeenAt"

var (
	// ErrNoCommonContacts is returned when the contacts service is disabled.
	ErrNoCommonContacts = errors.New("the organization instance does not read the common contacts")
	// ErrSinceTooOld is returned when since is older than the seen markers.
	ErrSinceTooOld = fmt.Errorf("since must be less than %s ago", maxReconcileAge)
	// ErrSinceInFuture prevents interpreting every contact as stale.
	ErrSinceInFuture = errors.New("since cannot be in the future")
	// ErrIncompleteRepublication prevents deletion before the expected contacts arrive.
	ErrIncompleteRepublication = errors.New("observed contacts do not match the completed republication")
)

// MarkContactSeen records an observation in the contact before it is saved.
// Keeping it in the same revision makes cache loss and concurrent updates safe.
func MarkContactSeen(c *contact.Contact) {
	c.M[commonContactsSeenAtKey] = time.Now().UTC().Format(time.RFC3339Nano)
}

// ReconcileReport tells which contacts of the organization instance were
// removed, or would be on a dry run, and how many were kept.
type ReconcileReport struct {
	Domain  string   `json:"domain"`
	Removed []string `json:"removed"`
	Kept    int      `json:"kept"`
}

// ReconcileOrganizationContacts deletes the contacts the feed wrote on the
// organization instance and did not send again since the given time, to catch
// the DELETE messages the stack missed.
func ReconcileOrganizationContacts(ctx context.Context, organizationID string, since time.Time, expectedContacts int, dryRun bool) (*ReconcileReport, error) {
	if time.Since(since) > maxReconcileAge {
		return nil, ErrSinceTooOld
	}
	if since.After(time.Now()) {
		return nil, ErrSinceInFuture
	}
	if expectedContacts < 0 {
		return nil, ErrIncompleteRepublication
	}
	orgInst, err := FindOrganizationInstance(ctx, organizationID, "")
	if err != nil {
		return nil, err
	}
	if orgInst == nil {
		return nil, fmt.Errorf("organization %s has no organization instance: %w", organizationID, instance.ErrNotFound)
	}
	if !orgInst.HasCommonContacts() {
		return nil, ErrNoCommonContacts
	}

	docs, err := findAllDocs[contact.Contact](orgInst, consts.Contacts, &couchdb.FindRequest{
		UseIndex: "by-carddav-path",
		Selector: mango.Gt(contact.CardDAVPathKey, ""),
	})
	if err != nil {
		return nil, err
	}
	report := &ReconcileReport{Domain: orgInst.Domain, Removed: []string{}}
	var stale []*contact.Contact
	for _, doc := range docs {
		var at time.Time
		if value, exists := doc.M[commonContactsSeenAtKey]; exists {
			text, _ := value.(string)
			at, err = time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return nil, fmt.Errorf("invalid contact observation for %s: %w", doc.ID(), err)
			}
		}
		if !at.Before(since) {
			report.Kept++
		} else {
			stale = append(stale, doc)
		}
	}
	if report.Kept != expectedContacts {
		return nil, fmt.Errorf("%w: got %d, expected %d; refusing to remove %d contacts", ErrIncompleteRepublication, report.Kept, expectedContacts, len(stale))
	}

	for _, doc := range stale {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		path, _ := doc.Get(contact.CardDAVPathKey).(string)
		// DeleteDoc, like a DELETE from the feed, so the share-group trigger
		// removes the person from the sharings of their groups.
		if !dryRun {
			if err := couchdb.DeleteDoc(orgInst, doc); err != nil {
				return report, fmt.Errorf("%s: %w", path, err)
			}
		}
		report.Removed = append(report.Removed, path)
	}
	return report, nil
}
