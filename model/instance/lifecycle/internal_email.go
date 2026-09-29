package lifecycle

import (
	"fmt"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// SetInternalEmail stores the internal email of the instance owner.
func SetInternalEmail(inst *instance.Instance, email string) error {
	email = utils.NormalizeEmail(email)
	if email == "" || email == inst.InternalEmail {
		return nil
	}
	inst.InternalEmail = email
	return update(inst)
}

// GetInstanceByInternalEmail retrieves an instance by its internal email.
func GetInstanceByInternalEmail(email string) (*instance.Instance, error) {
	var docs []*instance.Instance
	err := couchdb.FindDocs(prefixer.GlobalPrefixer, consts.Instances, &couchdb.FindRequest{
		UseIndex: "by-internalemail",
		Selector: mango.Equal("internal_email", utils.NormalizeEmail(email)),
		Limit:    2,
	}, &docs)
	if err != nil {
		return nil, err
	}
	switch len(docs) {
	case 0:
		return nil, instance.ErrNotFound
	case 1:
		return GetInstance(docs[0].Domain)
	default:
		return nil, fmt.Errorf("internal email %s is on several instances", email)
	}
}

// InternalEmailsReport sums up a backfill of internal emails.
type InternalEmailsReport struct {
	Scanned         int                 `json:"scanned"`
	MissingSettings []string            `json:"missing_settings"`
	EmptyEmails     int                 `json:"empty_emails"`
	Duplicates      map[string][]string `json:"duplicates"`
	Updated         int                 `json:"updated"`
	Skipped         int                 `json:"skipped"`
	Errors          []string            `json:"errors"`
}

// BackfillInternalEmails sets the internal email from the settings email. It
// writes nothing if a settings read fails, as it may hide a duplicate.
func BackfillInternalEmails(insts []*instance.Instance, dryRun bool) *InternalEmailsReport {
	report := &InternalEmailsReport{
		MissingSettings: []string{},
		Duplicates:      map[string][]string{},
		Errors:          []string{},
	}
	byEmail := make(map[string][]*instance.Instance)
	for _, inst := range insts {
		report.Scanned++
		if inst.IsOrganizationInstance() && inst.InternalEmail == "" {
			report.Skipped++
			continue
		}
		email := inst.InternalEmail
		if email == "" {
			var err error
			email, err = inst.SettingsEMail()
			if couchdb.IsNotFoundError(err) {
				report.MissingSettings = append(report.MissingSettings, inst.Domain)
				continue
			}
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", inst.Domain, err))
				continue
			}
		}
		if email = utils.NormalizeEmail(email); email == "" {
			report.EmptyEmails++
			continue
		}
		byEmail[email] = append(byEmail[email], inst)
	}

	scanFailed := len(report.Errors) > 0
	for email, owners := range byEmail {
		if len(owners) > 1 {
			for _, owner := range owners {
				report.Duplicates[email] = append(report.Duplicates[email], owner.Domain)
			}
			report.Skipped += len(owners)
			continue
		}
		inst := owners[0]
		if inst.InternalEmail == email || scanFailed {
			report.Skipped++
			continue
		}
		if !dryRun {
			if err := SetInternalEmail(inst, email); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", inst.Domain, err))
				continue
			}
		}
		report.Updated++
	}
	return report
}
