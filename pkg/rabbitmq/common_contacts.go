package rabbitmq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// CommonContactsHandler writes the contacts published on twake:contacts:common.
type CommonContactsHandler struct{}

// NewCommonContactsHandler creates a new common contacts handler.
func NewCommonContactsHandler() *CommonContactsHandler {
	return &CommonContactsHandler{}
}

// CommonContactMessage is a contact change published on twake:contacts:common.
type CommonContactMessage struct {
	Audience struct {
		User   string `json:"user"`
		Domain string `json:"domain"`
	} `json:"audience"`
	Action  string     `json:"action"`
	Path    string     `json:"path"`
	Payload *jsContact `json:"payload"`
}

// jsContact is the part of a JSContact card (RFC 9553) the stack keeps.
type jsContact struct {
	Name struct {
		Full       string `json:"full"`
		Components []struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		} `json:"components"`
	} `json:"name"`
	Emails     map[string]jsEntry `json:"emails"`
	Phones     map[string]jsEntry `json:"phones"`
	VCardProps [][]interface{}    `json:"vCardProps"`
}

// jsEntry is an email, with an address, or a phone, with a number.
type jsEntry struct {
	Address string `json:"address"`
	Number  string `json:"number"`
	Pref    int    `json:"pref"`
}

// Handle writes, updates or deletes the contact a message is about. The feed
// carries every user and domain, so a message for nobody here is acked.
func (h *CommonContactsHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	var msg CommonContactMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("contacts.common: %w", err)
	}
	if msg.Path == "" {
		return fmt.Errorf("contacts.common: missing path")
	}

	var inst *instance.Instance
	var err error
	switch {
	case msg.Audience.Domain != "":
		inst, err = orgdirectory.FindOrganizationInstance(ctx, "", msg.Audience.Domain)
		if err == nil && inst == nil {
			err = instance.ErrNotFound
		}
	case msg.Audience.User != "":
		inst, err = lifecycle.GetInstanceByEmail(msg.Audience.User)
	default:
		log.Infof("contacts.common: dropping %s without audience", msg.Path)
		return nil
	}
	if errors.Is(err, instance.ErrNotFound) {
		log.Debugf("contacts.common: no instance for %s, dropping %s", msg.Path, msg.Action)
		return nil
	}
	if err != nil {
		return err
	}
	if !inst.HasCommonContacts() {
		return nil
	}

	switch msg.Action {
	case "ADD", "UPDATE":
		if msg.Payload == nil {
			return fmt.Errorf("contacts.common: missing payload for %s", msg.Path)
		}
		return upsertCommonContact(inst, msg.Path, msg.Payload)
	case "DELETE":
		c, err := contact.FindByCardDAVPath(inst, msg.Path)
		if errors.Is(err, contact.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return couchdb.DeleteDoc(inst, c)
	default:
		return fmt.Errorf("contacts.common: unknown action %q for %s", msg.Action, msg.Path)
	}
}

func upsertCommonContact(inst *instance.Instance, path string, card *jsContact) error {
	emails := byPref(card.Emails)
	c, err := contact.FindByCardDAVPath(inst, path)
	if errors.Is(err, contact.ErrNotFound) && len(emails) > 0 {
		c, err = contact.FindExternalWithoutCardDAVPath(inst, emails[0])
	}
	if errors.Is(err, contact.ErrNotFound) {
		c, err = contact.New(), nil
		c.M["metadata"] = map[string]interface{}{"external": true}
	}
	if err != nil {
		return err
	}

	before, err := json.Marshal(c.M)
	if err != nil {
		return err
	}
	applyCard(c, path, card, emails)
	if c.Rev() == "" {
		return couchdb.CreateDoc(inst, c)
	}
	after, err := json.Marshal(c.M)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		return nil
	}
	return couchdb.UpdateDoc(inst, c)
}

// applyCard overwrites the fields that come from Sabre. The others, like the
// cozy URL, trustedForSharing or the groups, belong to the stack.
func applyCard(c *contact.Contact, path string, card *jsContact, emails []string) {
	c.M[contact.CardDAVPathKey] = path

	name := map[string]interface{}{}
	for _, comp := range card.Name.Components {
		switch comp.Kind {
		case "given":
			name["givenName"] = comp.Value
		case "surname":
			name["familyName"] = comp.Value
		}
	}
	full := strings.TrimSpace(card.Name.Full)
	if full == "" {
		given, _ := name["givenName"].(string)
		family, _ := name["familyName"].(string)
		full = strings.TrimSpace(given + " " + family)
	}
	setOrDelete(c.M, "name", name, len(name) > 0)
	setOrDelete(c.M, "fullname", full, full != "")

	var emailList []map[string]interface{}
	for i, address := range emails {
		emailList = append(emailList, map[string]interface{}{"address": address, "primary": i == 0})
	}
	setOrDelete(c.M, "email", emailList, len(emailList) > 0)

	var phoneList []map[string]interface{}
	for i, number := range byPref(card.Phones) {
		phoneList = append(phoneList, map[string]interface{}{"number": number, "primary": i == 0})
	}
	setOrDelete(c.M, "phone", phoneList, len(phoneList) > 0)

	displayName := full
	if displayName == "" && len(emails) > 0 {
		displayName = emails[0]
	}
	setOrDelete(c.M, "displayName", displayName, displayName != "")

	if c.PrimaryCozyURL() == "" {
		if host := utils.ExtractInstanceHost(card.vCardProp("x-twake-workplace-fqdn")); host != "" {
			c.M["cozy"] = []interface{}{map[string]interface{}{"url": "https://" + host, "primary": true}}
		}
	}

	index := c.PrimaryCozyURL()
	if index == "" && len(emails) > 0 {
		index = emails[0]
	}
	setOrDelete(c.M, "indexes", map[string]interface{}{"byFamilyNameGivenNameEmailCozyUrl": index}, index != "")
}

func setOrDelete(m map[string]interface{}, key string, value interface{}, ok bool) {
	if ok {
		m[key] = value
	} else {
		delete(m, key)
	}
}

// byPref returns the values of the entries by preference, a missing pref
// coming last.
func byPref(entries map[string]jsEntry) []string {
	prefs := map[string]int{}
	for _, e := range entries {
		if v := strings.TrimSpace(e.Address + e.Number); v != "" {
			prefs[v] = e.Pref
		}
	}
	values := make([]string, 0, len(prefs))
	for v := range prefs {
		values = append(values, v)
	}
	rank := func(v string) int {
		if prefs[v] <= 0 {
			return 101 // RFC 9553 prefs go from 1 to 100
		}
		return prefs[v]
	}
	sort.Slice(values, func(i, j int) bool {
		if rank(values[i]) != rank(values[j]) {
			return rank(values[i]) < rank(values[j])
		}
		return values[i] < values[j]
	})
	return values
}

// vCardProp returns the value of a vCard property JSContact does not cover,
// stored as jCard: [name, params, type, value].
func (card *jsContact) vCardProp(name string) string {
	for _, prop := range card.VCardProps {
		if len(prop) == 4 {
			if n, _ := prop[0].(string); strings.EqualFold(n, name) {
				value, _ := prop[3].(string)
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}
