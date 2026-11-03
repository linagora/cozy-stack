package sharing

import (
	"errors"
	"strings"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
)

// The kinds of a suggestion, by where its contact was found.
const (
	SuggestionOrgMember = "org_member"
	SuggestionContact   = "contact"
)

// A Suggestion is a group or person the user can share with. Contact is nil
// when no document matches the email: the client then shares by email.
type Suggestion struct {
	Contact     *contact.Contact
	Group       *contact.Group
	MemberCount int
	Kind        string
	Name        string
	Email       string
}

// Suggest returns matching groups followed by people. Groups come from the
// local database, plus the organization one in centralized mode. People come
// from the calendar side service when common_contacts is on, and from the same
// databases as groups otherwise or when the service fails. The user is left out.
func Suggest(inst *instance.Instance, q string, limit int) ([]Suggestion, error) {
	log := inst.Logger().WithNamespace("contacts")
	var orgInst *instance.Instance
	if inst.HasCentralizedOrgDirectory() {
		var err error
		if orgInst, err = findOrgInstance(inst); err != nil {
			log.Warnf("Cannot find the org instance: %s", err)
		}
	}
	suggestions, err := suggestGroups(inst, orgInst, q, limit)
	if err != nil || len(suggestions) >= limit {
		return suggestions, err
	}
	self, _ := inst.SettingsEMail()
	seen := map[string]bool{strings.ToLower(self): true}

	people, err := contact.Search(inst, q, limit)
	if err != nil {
		if !errors.Is(err, contact.ErrNoSearch) {
			log.Warnf("Cannot search the contacts, falling back to the databases: %s", err)
		}
		people, err := suggestFromDatabases(inst, orgInst, q, limit, seen)
		suggestions = append(suggestions, people...)
		return suggestions[:min(len(suggestions), limit)], err
	}

	emails := make([]string, len(people))
	for i, p := range people {
		emails[i] = p.Email
	}
	members := map[string]*contact.Contact{}
	if orgInst != nil {
		if members, err = contact.FindByEmails(orgInst, emails); err != nil {
			return nil, err
		}
	}
	contacts, err := contact.FindByEmails(inst, emails)
	if err != nil {
		return nil, err
	}

	for _, p := range people {
		if len(suggestions) >= limit {
			break
		}
		s := Suggestion{Name: p.Name, Email: p.Email}
		key := strings.ToLower(p.Email)
		if c, ok := members[key]; ok {
			s.Contact, s.Kind = c, SuggestionOrgMember
		} else if c, ok := contacts[key]; ok {
			s.Contact, s.Kind = c, SuggestionContact
		}
		suggestions = appendSuggestion(suggestions, seen, s)
	}
	return suggestions, nil
}

// suggestGroups returns the organization groups, then the local ones. In
// centralized mode, local copies of organization groups are stale and skipped.
func suggestGroups(inst, orgInst *instance.Instance, q string, limit int) ([]Suggestion, error) {
	var suggestions []Suggestion
	// A failed search is only logged, so that people are still suggested.
	add := func(db *instance.Instance, groups []*contact.Group, err error) error {
		if err != nil {
			inst.Logger().WithNamespace("contacts").Warnf("Cannot search contact groups: %s", err)
			return nil
		}
		ids := make([]string, len(groups))
		for i, group := range groups {
			ids[i] = group.ID()
		}
		counts, err := contact.CountGroupMembers(db, ids)
		if err != nil {
			return err
		}
		for _, group := range groups {
			suggestions = append(suggestions, Suggestion{Group: group, MemberCount: counts[group.ID()]})
		}
		return nil
	}
	if orgInst != nil {
		groups, err := orgdirectory.FindGroupsByText(orgInst, orgInst.OrgID, q, limit)
		if err := add(orgInst, groups, err); err != nil {
			return nil, err
		}
	}
	if len(suggestions) >= limit {
		return suggestions, nil
	}
	var filters []mango.Filter
	if inst.HasCentralizedOrgDirectory() {
		filters = append(filters, orgdirectory.NotManagedGroup())
	}
	groups, err := contact.FindGroupsByText(inst, q, limit-len(suggestions), filters...)
	if err := add(inst, groups, err); err != nil {
		return nil, err
	}
	return suggestions, nil
}

// suggestFromDatabases searches people by name or email in the organization
// database, when there is one, then in the local one.
func suggestFromDatabases(inst, orgInst *instance.Instance, q string, limit int, seen map[string]bool) ([]Suggestion, error) {
	var members []*contact.Contact
	if orgInst != nil {
		var err error
		if members, err = contact.FindByText(orgInst, q, limit); err != nil {
			return nil, err
		}
	}
	contacts, err := contact.FindByText(inst, q, limit)
	if err != nil {
		return nil, err
	}

	var suggestions []Suggestion
	add := func(c *contact.Contact, kind string) {
		if len(suggestions) >= limit {
			return
		}
		if addr, err := c.ToMailAddress(); err == nil {
			s := Suggestion{Contact: c, Kind: kind, Name: addr.Name, Email: addr.Email}
			suggestions = appendSuggestion(suggestions, seen, s)
		}
	}
	// A member copied to the instance is found on both: the org one wins.
	for _, c := range members {
		add(c, SuggestionOrgMember)
	}
	for _, c := range contacts {
		add(c, SuggestionContact)
	}
	return suggestions, nil
}

// appendSuggestion skips the emails already seen, case-insensitively.
func appendSuggestion(suggestions []Suggestion, seen map[string]bool, s Suggestion) []Suggestion {
	key := strings.ToLower(s.Email)
	if seen[key] {
		return suggestions
	}
	seen[key] = true
	return append(suggestions, s)
}
