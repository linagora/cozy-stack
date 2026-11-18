package sharing

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// personalContactsPerJob bounds the size of a RabbitMQ message. The job queue
// paces the publication.
const personalContactsPerJob = 100

// PersonalContactsReport sums up a publication of the personal contacts.
type PersonalContactsReport struct {
	Scanned   int      `json:"scanned"`
	Skipped   int      `json:"skipped"`
	Published int      `json:"published"`
	Errors    []string `json:"errors"`
}

// PublishPersonalContacts sends to Sabre the personal contacts that only exist
// in the stack. Each one is marked external first, so that the card coming
// back from the feed attaches to it instead of creating a duplicate. It is safe
// to run twice: a contact that came back has a CardDAV path and is left out.
func PublishPersonalContacts(insts []*instance.Instance, dryRun bool) *PersonalContactsReport {
	report := &PersonalContactsReport{Errors: []string{}}
	for _, inst := range insts {
		report.Scanned++
		if !collectedContactsEnabled(inst, 1) {
			report.Skipped++
			continue
		}
		n, err := publishPersonalContacts(inst, dryRun)
		report.Published += n
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", inst.Domain, err))
		}
	}
	return report
}

func publishPersonalContacts(inst *instance.Instance, dryRun bool) (int, error) {
	var candidates []*contact.Contact
	claimed := map[string]bool{}
	err := couchdb.ForeachDocs(inst, consts.Contacts, func(_ string, raw json.RawMessage) error {
		doc := contact.New()
		if err := json.Unmarshal(raw, doc); err != nil {
			return err
		}
		if isPersonalContactOnlyInStack(doc) {
			candidates = append(candidates, doc)
		} else if _, ok := doc.M[contact.CardDAVPathKey]; ok || doc.IsExternal() || orgdirectory.IsManagedDirectoryDoc(&doc.JSONDoc) {
			claim(claimed, doc)
		}
		return nil
	})
	if couchdb.IsNoDatabaseError(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	// A second external contact for an email would never get the card back,
	// and breaks the lookups that expect a single external contact per email.
	// The ones already external go first, as the feed may attach to them.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].IsExternal() && !candidates[j].IsExternal()
	})
	var docs []*contact.Contact
	for _, doc := range candidates {
		if isClaimed(claimed, doc) {
			continue
		}
		claim(claimed, doc)
		docs = append(docs, doc)
	}
	if dryRun {
		return len(docs), nil
	}

	published := 0
	for chunk := range slices.Chunk(docs, personalContactsPerJob) {
		batch := make([]CollectedContact, 0, len(chunk))
		for _, doc := range chunk {
			if !doc.IsExternal() {
				metadata, _ := doc.M["metadata"].(map[string]interface{})
				if metadata == nil {
					metadata = map[string]interface{}{}
				}
				metadata["external"] = true
				doc.M["metadata"] = metadata
				if err := couchdb.UpdateDoc(inst, doc); err != nil {
					return published, err
				}
			}
			batch = append(batch, toCollectedContact(doc))
		}
		msg, err := job.NewMessage(&CollectedContactsMsg{Contacts: batch})
		if err == nil {
			_, err = job.System().PushJob(inst, &job.JobRequest{
				WorkerType: CollectedContactsWorker,
				Message:    msg,
			})
		}
		if err != nil {
			return published, err
		}
		published += len(batch)
	}
	return published, nil
}

// isPersonalContactOnlyInStack leaves out the contacts that came from the
// feed, the member copies, the user's own contact, and the ones that cannot
// be invited.
func isPersonalContactOnlyInStack(doc *contact.Contact) bool {
	if _, ok := doc.M[contact.CardDAVPathKey]; ok {
		return false
	}
	if doc.Get("me") == true || doc.Get("trashed") == true {
		return false
	}
	if orgdirectory.IsManagedDirectoryDoc(&doc.JSONDoc) {
		return false
	}
	return len(entries(doc, "email", "address")) > 0
}

func claim(claimed map[string]bool, doc *contact.Contact) {
	for _, email := range entries(doc, "email", "address") {
		claimed[strings.ToLower(email)] = true
	}
}

func isClaimed(claimed map[string]bool, doc *contact.Contact) bool {
	for _, email := range entries(doc, "email", "address") {
		if claimed[strings.ToLower(email)] {
			return true
		}
	}
	return false
}

func toCollectedContact(doc *contact.Contact) CollectedContact {
	name, _ := doc.Get("name").(map[string]interface{})
	given, _ := name["givenName"].(string)
	family, _ := name["familyName"].(string)
	return CollectedContact{
		Name:       doc.PrimaryName(),
		GivenName:  strings.TrimSpace(given),
		FamilyName: strings.TrimSpace(family),
		Emails:     entries(doc, "email", "address"),
		Phones:     entries(doc, "phone", "number"),
	}
}

// entries returns the values of a list field, the primary one first.
func entries(doc *contact.Contact, field, key string) []string {
	list, _ := doc.Get(field).([]interface{})
	var values []string
	for _, item := range list {
		obj, _ := item.(map[string]interface{})
		value, _ := obj[key].(string)
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if primary, _ := obj["primary"].(bool); primary {
			values = append([]string{value}, values...)
		} else {
			values = append(values, value)
		}
	}
	return values
}
