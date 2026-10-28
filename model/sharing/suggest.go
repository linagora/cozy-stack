package sharing

import (
	"errors"
	"strings"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/orgdirectory"
)

// The kinds of a suggestion, by where its contact was found.
const (
	SuggestionMember  = "member"
	SuggestionContact = "contact"
)

// A Suggestion is a group or person the user can share with. Contact is nil
// when no document matches the email: the client then shares by email.
type Suggestion struct {
	Contact *contact.Contact
	Group   *contact.Group
	Kind    string
	Name    string
	Email   string
}

// Suggest returns matching groups followed by people. Without a centralized
// directory, only the instance's local databases are searched. With one, groups
// come from the organization and people from the calendar side service, falling
// back to the organization and local contacts. The user is left out.
func Suggest(inst *instance.Instance, q string, limit int) ([]Suggestion, error) {
	log := inst.Logger().WithNamespace("contacts")
	centralized := inst.HasCentralizedOrgDirectory()
	var orgInst *instance.Instance
	if centralized {
		var err error
		orgInst, err = findOrgInstance(inst)
		if err != nil {
			log.Warnf("Cannot find the org instance: %s", err)
		}
	}
	var groups []*contact.Group
	var err error
	if !centralized {
		groups, err = contact.FindGroupsByText(inst, q, limit)
	} else if orgInst != nil {
		groups, err = orgdirectory.FindGroupsByText(orgInst, orgInst.OrgID, q, limit)
	}
	var suggestions []Suggestion
	if err != nil {
		log.Warnf("Cannot search contact groups: %s", err)
	} else {
		for _, group := range groups {
			suggestions = append(suggestions, Suggestion{Group: group})
		}
	}
	if len(suggestions) >= limit {
		return suggestions, nil
	}
	self, _ := inst.SettingsEMail()
	seen := map[string]bool{strings.ToLower(self): true}

	var people []contact.Person
	err = contact.ErrNoSearch
	if centralized {
		people, err = contact.Search(inst, q, limit)
	}
	if err != nil {
		if !errors.Is(err, contact.ErrNoSearch) {
			log.Warnf("Cannot search the contacts, falling back to the local search: %s", err)
		}
		people, err := suggestLocally(inst, orgInst, q, limit, seen)
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
			s.Contact, s.Kind = c, SuggestionMember
		} else if c, ok := contacts[key]; ok {
			s.Contact, s.Kind = c, SuggestionContact
		}
		suggestions = appendSuggestion(suggestions, seen, s)
	}
	return suggestions, nil
}

func suggestLocally(inst, orgInst *instance.Instance, q string, limit int, seen map[string]bool) ([]Suggestion, error) {
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
		add(c, SuggestionMember)
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
