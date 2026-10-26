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

var (
	// ErrNoCommonContacts is returned when the contacts service is disabled.
	ErrNoCommonContacts = errors.New("the organization instance does not read the common contacts")
	// ErrNoCentralizedOrgDirectory prevents cleanup while replication is retained.
	ErrNoCentralizedOrgDirectory = errors.New("the organization directory is not centralized")
	// ErrOrgInstanceNotFilled prevents deleting a copy absent from the org instance.
	ErrOrgInstanceNotFilled = errors.New("the organization instance does not contain every member copy")
)

// MemberCopiesReport tells how many member copies were removed on each
// instance, or would be on a dry run.
type MemberCopiesReport struct {
	Removed map[string]int `json:"removed"`
	Errors  []string       `json:"errors,omitempty"`
}

// RemoveMemberCopies deletes the members copied by the organization directory
// on the member instances of an organization, once its directory is centralized
// and populated. Personal contacts, contacts
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
	if !orgInst.HasCentralizedOrgDirectory() {
		return nil, ErrNoCentralizedOrgDirectory
	}

	report := &MemberCopiesReport{Removed: map[string]int{}}
	candidates := make(map[*instance.Instance][]couchdb.Doc)
	verified := make(map[[2]string]bool)
	for _, inst := range scope.Instances {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if inst == orgInst || !inst.HasCentralizedOrgDirectory() {
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
				// Verify every destination before deleting any source, regardless
				// of whether B2B events or the contacts service filled the org.
				input := contactPatchFromContact(scope.OrganizationID, doc)
				identity := [2]string{input.Email, input.CozyURL}
				if !verified[identity] {
					_, err := findManagedContact(orgInst, input)
					if errors.Is(err, contact.ErrNotFound) {
						return nil, fmt.Errorf("%w: %s on %s", ErrOrgInstanceNotFilled, doc.ID(), inst.Domain)
					}
					if err != nil {
						return nil, fmt.Errorf("verify org contact for %s on %s: %w", doc.ID(), inst.Domain, err)
					}
					verified[identity] = true
				}
				copies = append(copies, doc)
			}
		}
		if len(copies) == 0 {
			continue
		}
		candidates[inst] = copies
	}
	for _, inst := range scope.Instances {
		copies := candidates[inst]
		if len(copies) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return report, err
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
