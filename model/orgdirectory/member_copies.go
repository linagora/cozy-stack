package orgdirectory

import (
	"context"
	"errors"
	"fmt"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// MemberCopiesReport tells how many member copies were removed on each
// instance, or would be on a dry run.
type MemberCopiesReport struct {
	Removed map[string]int `json:"removed"`
	Errors  []string       `json:"errors,omitempty"`
}

// RemoveMemberCopies deletes the members copied by the organization directory
// on the member instances of an organization, once the twake:contacts:common
// feed has filled its organization instance. Personal contacts, contacts
// written for sharing, and copies the feed took over are kept.
func RemoveMemberCopies(ctx context.Context, organizationID string, dryRun bool) (*MemberCopiesReport, error) {
	scope, err := ResolveOrganizationInstances(organizationID, "")
	if err != nil {
		return nil, err
	}
	var orgInst *instance.Instance
	for _, inst := range scope.Instances {
		if inst.IsOrganizationInstance() {
			orgInst = inst
		}
	}
	if orgInst == nil {
		return nil, fmt.Errorf("organization %s has no organization instance: %w", organizationID, instance.ErrNotFound)
	}
	if !orgInst.HasCommonContacts() {
		return nil, errors.New("the organization instance does not read the common contacts")
	}

	report := &MemberCopiesReport{Removed: map[string]int{}}
	for _, inst := range scope.Instances {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		// Without the feed, user.created still writes the copies back.
		if inst == orgInst || !inst.HasCommonContacts() {
			continue
		}
		docs, err := listManagedContacts(inst, scope.OrganizationID)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", inst.Domain, err))
			continue
		}
		var copies []couchdb.Doc
		for _, doc := range docs {
			if _, ok := doc.M[contact.CardDAVPathKey]; !ok {
				copies = append(copies, doc)
			}
		}
		if len(copies) == 0 {
			continue
		}
		// BulkDeleteDocs publishes no old document, so the share-group
		// trigger does not revoke the members of the groups.
		if !dryRun {
			if err := couchdb.BulkDeleteDocs(inst, consts.Contacts, copies); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", inst.Domain, err))
				continue
			}
		}
		report.Removed[inst.Domain] = len(copies)
	}
	return report, nil
}
