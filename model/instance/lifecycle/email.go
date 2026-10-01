package lifecycle

import (
	"errors"
	"fmt"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// ErrEmailTaken is returned when the email is already on another instance.
var ErrEmailTaken = errors.New("email is already on another instance")

// SetEmail stores the email of the instance owner. The lock on the email
// keeps two instances from taking it at the same time.
func SetEmail(inst *instance.Instance, email string) error {
	email = utils.NormalizeEmail(email)
	if email == "" || email == inst.Email {
		return nil
	}
	mu := config.Lock().ReadWrite(prefixer.GlobalPrefixer, "instance-email/"+email)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	docs, err := findByEmail(email)
	if err != nil {
		return err
	}
	for _, doc := range docs {
		if doc.Domain != inst.Domain {
			return fmt.Errorf("%w: %s", ErrEmailTaken, doc.Domain)
		}
	}
	inst.Email = email
	return update(inst)
}

// GetInstanceByEmail retrieves an instance by its email.
func GetInstanceByEmail(email string) (*instance.Instance, error) {
	docs, err := findByEmail(utils.NormalizeEmail(email))
	if err != nil {
		return nil, err
	}
	switch len(docs) {
	case 0:
		return nil, instance.ErrNotFound
	case 1:
		return GetInstance(docs[0].Domain)
	default:
		return nil, fmt.Errorf("email %s is on several instances", email)
	}
}

// findByEmail returns at most two instances, enough to spot a duplicate.
func findByEmail(email string) ([]*instance.Instance, error) {
	var docs []*instance.Instance
	err := couchdb.FindDocs(prefixer.GlobalPrefixer, consts.Instances, &couchdb.FindRequest{
		UseIndex: "by-email",
		Selector: mango.Equal("email", email),
		Limit:    2,
	}, &docs)
	return docs, err
}

// EmailsReport sums up a backfill of emails.
type EmailsReport struct {
	Scanned         int                 `json:"scanned"`
	MissingSettings []string            `json:"missing_settings"`
	EmptyEmails     int                 `json:"empty_emails"`
	Duplicates      map[string][]string `json:"duplicates"`
	Updated         int                 `json:"updated"`
	Skipped         int                 `json:"skipped"`
	Errors          []string            `json:"errors"`
}

// isOrgWithoutEmail skips org instances that have no email; one that already
// holds an email stays in the scan so it can still block a member sharing it.
func isOrgWithoutEmail(inst *instance.Instance) bool {
	return inst.IsOrganizationInstance() && inst.Email == ""
}

// BackfillEmails sets the email from the settings email. It
// writes nothing if a settings read fails, as it may hide a duplicate.
func BackfillEmails(insts []*instance.Instance, dryRun bool) *EmailsReport {
	report := &EmailsReport{
		MissingSettings: []string{},
		Duplicates:      map[string][]string{},
		Errors:          []string{},
	}
	byEmail := make(map[string][]*instance.Instance)
	for _, inst := range insts {
		report.Scanned++
		if isOrgWithoutEmail(inst) {
			report.Skipped++
			continue
		}
		email := inst.Email
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
		if inst.Email == email || scanFailed {
			report.Skipped++
			continue
		}
		if !dryRun {
			err := SetEmail(inst, email)
			if errors.Is(err, ErrEmailTaken) {
				report.Duplicates[email] = append(report.Duplicates[email], inst.Domain)
				report.Skipped++
				continue
			}
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", inst.Domain, err))
				continue
			}
		}
		report.Updated++
	}
	return report
}
