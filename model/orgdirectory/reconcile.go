package orgdirectory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
)

// seenTTL is how long the stack remembers that the feed sent a contact. A
// reconciliation can only look back that far.
const seenTTL = 7 * 24 * time.Hour

func seenKey(inst *instance.Instance, path string) string {
	return "common-contacts-seen:" + inst.Domain + ":" + path
}

// MarkContactSeen remembers that twake:contacts:common sent the contact at
// path for the organization instance, even when nothing was written for it.
func MarkContactSeen(inst *instance.Instance, path string) {
	at := strconv.FormatInt(time.Now().UnixMilli(), 10)
	config.GetConfig().CacheStorage.Set(seenKey(inst, path), []byte(at), seenTTL)
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
func ReconcileOrganizationContacts(ctx context.Context, organizationID string, since time.Time, dryRun bool) (*ReconcileReport, error) {
	if time.Since(since) > seenTTL {
		return nil, fmt.Errorf("since must be less than %s ago", seenTTL)
	}
	orgInst, err := FindOrganizationInstance(ctx, organizationID, "")
	if err != nil {
		return nil, err
	}
	if orgInst == nil {
		return nil, fmt.Errorf("organization %s has no organization instance: %w", organizationID, instance.ErrNotFound)
	}
	if !orgInst.HasCommonContacts() {
		return nil, errors.New("the organization instance does not read the common contacts")
	}

	docs, err := findAllDocs[contact.Contact](orgInst, consts.Contacts, &couchdb.FindRequest{
		UseIndex: "by-carddav-path",
		Selector: mango.Gt(contact.CardDAVPathKey, ""),
	})
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(docs))
	for i, doc := range docs {
		path, _ := doc.Get(contact.CardDAVPathKey).(string)
		keys[i] = seenKey(orgInst, path)
	}
	seen := config.GetConfig().CacheStorage.MultiGet(keys)

	report := &ReconcileReport{Domain: orgInst.Domain, Removed: []string{}}
	var stale []*contact.Contact
	for i, doc := range docs {
		at, _ := strconv.ParseInt(string(seen[i]), 10, 64)
		if at >= since.UnixMilli() {
			report.Kept++
		} else {
			stale = append(stale, doc)
		}
	}
	// Nothing seen means the republication did not run or did not reach the
	// stack, not that Sabre is empty.
	if len(stale) > 0 && report.Kept == 0 {
		return nil, fmt.Errorf("no contact was sent since %s, refusing to remove %d contacts", since.Format(time.RFC3339), len(stale))
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
