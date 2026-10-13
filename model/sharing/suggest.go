package sharing

import (
	"errors"
	"strings"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
)

// The kinds of a suggestion, by where its contact was found.
const (
	SuggestionMember  = "member"
	SuggestionContact = "contact"
)

// A Suggestion is someone the user can share with. Contact is nil when no
// document matches the email: the client then shares by email.
type Suggestion struct {
	Contact *contact.Contact
	Kind    string
	Name    string
	Email   string
}

// Suggest returns the people matching q, in the order of the autocomplete API
// of the calendar side service. The user is left out.
func Suggest(inst *instance.Instance, q string, limit int) ([]Suggestion, error) {
	log := inst.Logger().WithNamespace("contacts")
	orgInst, err := findOrgInstance(inst)
	if err != nil {
		// The members are then suggested without their document.
		log.Warnf("Cannot find the org instance: %s", err)
	}
	self, _ := inst.SettingsEMail()
	seen := map[string]bool{strings.ToLower(self): true}

	people, err := contact.Search(inst, q, limit)
	if err != nil {
		if !errors.Is(err, contact.ErrNoSearch) {
			log.Warnf("Cannot search the contacts: %s", err)
		}
		return nil, nil
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

	var suggestions []Suggestion
	for _, p := range people {
		s := Suggestion{Name: p.Name, Email: p.Email}
		if c, ok := members[p.Email]; ok {
			s.Contact, s.Kind = c, SuggestionMember
		} else if c, ok := contacts[p.Email]; ok {
			s.Contact, s.Kind = c, SuggestionContact
		}
		suggestions = appendSuggestion(suggestions, seen, s)
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
