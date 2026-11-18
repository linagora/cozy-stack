package sharing

import (
	"context"
	"errors"
	"fmt"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/messaging"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
)

// CollectedContactsWorker is the worker that publishes collected contacts.
const CollectedContactsWorker = "share-collected-contacts"

var errNoCollectedPublisher = errors.New("sharing: no publisher for the collected contacts")

// CollectedContactsMsg is the job message: the emails of the contacts created
// by a sharing, and the full contacts sent by the personal contacts fixer.
type CollectedContactsMsg struct {
	Emails   []string           `json:"emails"`
	Contacts []CollectedContact `json:"contacts,omitempty"`
}

// CollectedContact holds the fields the common contacts feed writes back on
// the contact, so that publishing it loses nothing.
type CollectedContact struct {
	Name       string   `json:"name,omitempty"`
	GivenName  string   `json:"givenName,omitempty"`
	FamilyName string   `json:"familyName,omitempty"`
	Emails     []string `json:"emails"`
	Phones     []string `json:"phones,omitempty"`
}

// EnqueueCollectedContacts pushes the publish job. Errors are only logged.
func EnqueueCollectedContacts(inst *instance.Instance, emails []string) {
	if !collectedContactsEnabled(inst, len(emails)) {
		return
	}
	msg, err := job.NewMessage(&CollectedContactsMsg{Emails: emails})
	if err == nil {
		_, err = job.System().PushJob(inst, &job.JobRequest{
			WorkerType: CollectedContactsWorker,
			Message:    msg,
		})
	}
	if err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("Cannot schedule the publication of %d collected contacts: %s", len(emails), err)
	}
}

// PublishCollectedContacts sends the new contacts to Sabre in one message.
func PublishCollectedContacts(ctx context.Context, inst *instance.Instance, publisher messaging.Publisher, msg CollectedContactsMsg) error {
	if !collectedContactsEnabled(inst, len(msg.Emails)+len(msg.Contacts)) {
		return nil
	}
	if publisher == nil {
		return errNoCollectedPublisher
	}
	return publisher.Publish(ctx, collectedRequest(inst, msg))
}

// collectedContactsEnabled tells if there is something to publish.
func collectedContactsEnabled(inst *instance.Instance, count int) bool {
	return count > 0 && inst.HasCommonContacts() && inst.Email != ""
}

func collectedRequest(inst *instance.Instance, msg CollectedContactsMsg) messaging.PublishRequest {
	cards := make([]map[string]interface{}, 0, len(msg.Emails)+len(msg.Contacts))
	for _, email := range msg.Emails {
		cards = append(cards, map[string]interface{}{
			"@type":  "Card",
			"emails": map[string]interface{}{"e1": map[string]interface{}{"address": email}},
		})
	}
	for _, c := range msg.Contacts {
		cards = append(cards, fullCard(c))
	}
	return messaging.PublishRequest{
		ContextName: inst.ContextName,
		Exchange:    rabbitmq.ExchangeContactsCollected,
		Fanout:      true,
		Payload: rabbitmq.CollectedContactsMessage{
			UserEmail:         inst.Email,
			CollectedContacts: cards,
		},
	}
}

// fullCard writes the first email and phone as preferred, the order the feed
// reads them back in.
func fullCard(c CollectedContact) map[string]interface{} {
	card := map[string]interface{}{"@type": "Card"}
	name := map[string]interface{}{"@type": "Name"}
	if c.Name != "" {
		name["full"] = c.Name
	}
	var components []interface{}
	if c.GivenName != "" {
		components = append(components, map[string]interface{}{"kind": "given", "value": c.GivenName})
	}
	if c.FamilyName != "" {
		components = append(components, map[string]interface{}{"kind": "surname", "value": c.FamilyName})
	}
	if len(components) > 0 {
		name["components"] = components
	}
	if len(name) > 1 {
		card["name"] = name
	}
	list := func(prefix, key string, values []string) map[string]interface{} {
		m := make(map[string]interface{}, len(values))
		for i, v := range values {
			entry := map[string]interface{}{key: v}
			if i == 0 {
				entry["pref"] = 1
			}
			m[fmt.Sprintf("%s%d", prefix, i+1)] = entry
		}
		return m
	}
	card["emails"] = list("e", "address", c.Emails)
	if len(c.Phones) > 0 {
		card["phones"] = list("p", "number", c.Phones)
	}
	return card
}
