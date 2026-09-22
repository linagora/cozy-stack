package rabbitmq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

func TestCommonContactsHandler(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	const enabledCtx, disabledCtx = "common-contacts-on", "common-contacts-off"
	config.GetConfig().Contexts = map[string]interface{}{
		enabledCtx:  map[string]interface{}{"common_contacts": true},
		disabledCtx: map[string]interface{}{},
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "cc" + suffix
	orgDomain := "acme-" + suffix + ".example"
	newInstance := func(t *testing.T, domain, contextName string) *instance.Instance {
		t.Helper()
		inst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      domain,
			OrgDomain:   orgDomain,
			OrgID:       orgID,
			ContextName: contextName,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
		return inst
	}
	org := newInstance(t, orgID+".cc.localhost", enabledCtx)
	alice := newInstance(t, "alice-"+suffix+".cc.localhost", enabledCtx)
	aliceEmail := "alice-" + suffix + "@acme.example"
	require.NoError(t, lifecycle.SetInternalEmail(alice, aliceEmail))
	bob := newInstance(t, "bob-"+suffix+".cc.localhost", disabledCtx)
	bobEmail := "bob-" + suffix + "@acme.example"
	require.NoError(t, lifecycle.SetInternalEmail(bob, bobEmail))

	handle := func(t *testing.T, msg map[string]interface{}) {
		t.Helper()
		body, err := json.Marshal(msg)
		require.NoError(t, err)
		require.NoError(t, rabbitmq.NewCommonContactsHandler().
			Handle(context.Background(), amqp.Delivery{Body: body}))
	}
	card := func(full, email, phone, fqdn string) map[string]interface{} {
		c := map[string]interface{}{
			"@type": "Card",
			"name": map[string]interface{}{
				"full": full,
				"components": []interface{}{
					map[string]interface{}{"kind": "given", "value": "Carol"},
					map[string]interface{}{"kind": "surname", "value": "Doe"},
				},
			},
			"emails":     map[string]interface{}{"e1": map[string]interface{}{"address": email}},
			"vCardProps": []interface{}{[]interface{}{"version", map[string]interface{}{}, "text", "4.0"}},
		}
		if phone != "" {
			c["phones"] = map[string]interface{}{"p1": map[string]interface{}{"number": phone}}
		}
		if fqdn != "" {
			c["vCardProps"] = append(c["vCardProps"].([]interface{}),
				[]interface{}{"x-twake-workplace-fqdn", map[string]interface{}{}, "unknown", fqdn})
		}
		return c
	}
	message := func(action string, audience map[string]interface{}, path string, payload map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"action": action, "audience": audience, "path": path, "uid": "uid", "payload": payload}
	}
	byPath := func(t *testing.T, inst *instance.Instance, path string) []*contact.Contact {
		t.Helper()
		var docs []*contact.Contact
		err := couchdb.FindDocs(inst, consts.Contacts, &couchdb.FindRequest{
			UseIndex: "by-carddav-path",
			Selector: mango.Equal("carddavPath", path),
		}, &docs)
		if !couchdb.IsNoDatabaseError(err) {
			require.NoError(t, err)
		}
		return docs
	}
	domainAudience := map[string]interface{}{"domain": orgDomain}

	t.Run("a domain contact is written on the org instance", func(t *testing.T) {
		path := "addressbooks/domain/members/carol-" + suffix + ".vcf"
		handle(t, message("ADD", domainAudience, path, card("Carol Doe", "carol@acme.example", "+33123", "carol.cc.localhost")))

		docs := byPath(t, org, path)
		require.Len(t, docs, 1)
		c := docs[0]
		require.Equal(t, "Carol Doe", c.PrimaryName())
		require.Equal(t, "+33123", c.PrimaryPhoneNumber())
		require.Equal(t, "https://carol.cc.localhost", c.PrimaryCozyURL())
		require.True(t, c.IsExternal())
		addr, err := c.ToMailAddress()
		require.NoError(t, err)
		require.Equal(t, "carol@acme.example", addr.Email)
		require.Empty(t, byPath(t, alice, path))

		t.Run("replaying it changes nothing", func(t *testing.T) {
			handle(t, message("ADD", domainAudience, path, card("Carol Doe", "carol@acme.example", "+33123", "carol.cc.localhost")))
			again := byPath(t, org, path)
			require.Len(t, again, 1)
			require.Equal(t, c.Rev(), again[0].Rev())
		})

		t.Run("an update keeps the stack fields", func(t *testing.T) {
			c.M["cozy"] = []interface{}{map[string]interface{}{"url": "https://kept.example", "primary": true}}
			c.M[contact.TrustedForSharingKey] = true
			c.M["relationships"] = map[string]interface{}{"groups": map[string]interface{}{
				"data": []interface{}{map[string]interface{}{"_id": "g1", "_type": consts.Groups}},
			}}
			require.NoError(t, couchdb.UpdateDoc(org, c))

			handle(t, message("UPDATE", domainAudience, path, card("Carol Smith", "carol@acme.example", "", "carol.cc.localhost")))
			docs := byPath(t, org, path)
			require.Len(t, docs, 1)
			updated := docs[0]
			require.Equal(t, "Carol Smith", updated.PrimaryName())
			require.Empty(t, updated.PrimaryPhoneNumber())
			require.Equal(t, "https://kept.example", updated.PrimaryCozyURL())
			require.True(t, updated.IsTrusted())
			require.Equal(t, []string{"g1"}, updated.GroupIDs())
		})

		t.Run("a delete removes it", func(t *testing.T) {
			handle(t, message("DELETE", domainAudience, path, nil))
			require.Empty(t, byPath(t, org, path))
		})
	})

	t.Run("a member copied before the feed is taken over, not duplicated", func(t *testing.T) {
		email := "dave-" + suffix + "@acme.example"
		copied := createContact(t, org, email, "https://dave.cc.localhost", true, "Dave")
		path := "addressbooks/domain/members/dave-" + suffix + ".vcf"
		handle(t, message("ADD", domainAudience, path, card("Dave Doe", email, "", "")))

		docs := byPath(t, org, path)
		require.Len(t, docs, 1)
		require.Equal(t, copied.ID(), docs[0].ID())
		require.Equal(t, "Dave Doe", docs[0].PrimaryName())
		require.Equal(t, "https://dave.cc.localhost", docs[0].PrimaryCozyURL())
	})

	t.Run("a personal contact is written on its owner's instance", func(t *testing.T) {
		path := "addressbooks/alice/collected/erin-" + suffix + ".vcf"
		handle(t, message("ADD", map[string]interface{}{"user": aliceEmail}, path, card("Erin", "erin@other.example", "", "")))

		require.Len(t, byPath(t, alice, path), 1)
		require.Empty(t, byPath(t, org, path))
	})

	t.Run("the preferred email is the primary one", func(t *testing.T) {
		path := "addressbooks/alice/contacts/hugo-" + suffix + ".vcf"
		payload := card("Hugo", "", "", "")
		payload["emails"] = map[string]interface{}{
			"e1": map[string]interface{}{"address": "a-no-pref@other.example"},
			"e2": map[string]interface{}{"address": "z-pref@other.example", "pref": 1},
			"e3": map[string]interface{}{"address": "b-pref@other.example", "pref": 2},
		}
		handle(t, message("ADD", map[string]interface{}{"user": aliceEmail}, path, payload))

		docs := byPath(t, alice, path)
		require.Len(t, docs, 1)
		addr, err := docs[0].ToMailAddress()
		require.NoError(t, err)
		require.Equal(t, "z-pref@other.example", addr.Email)
		require.Equal(t, []interface{}{"z-pref@other.example", "b-pref@other.example", "a-no-pref@other.example"},
			func() (out []interface{}) {
				for _, e := range docs[0].M["email"].([]interface{}) {
					out = append(out, e.(map[string]interface{})["address"])
				}
				return
			}())
	})

	t.Run("messages for nobody here are acked and dropped", func(t *testing.T) {
		path := "addressbooks/x/collected/frank-" + suffix + ".vcf"
		payload := card("Frank", "frank@other.example", "", "")
		handle(t, message("ADD", map[string]interface{}{"user": bobEmail}, path, payload))
		handle(t, message("ADD", map[string]interface{}{}, path, payload))
		handle(t, message("ADD", map[string]interface{}{"user": "nobody-" + suffix + "@acme.example"}, path, payload))
		handle(t, message("ADD", map[string]interface{}{"domain": "unknown-" + suffix + ".example"}, path, payload))

		require.Empty(t, byPath(t, bob, path))
	})

	t.Run("malformed messages are rejected", func(t *testing.T) {
		alicePath := "addressbooks/alice/collected/gina-" + suffix + ".vcf"
		for _, body := range []interface{}{
			"{",
			message("ADD", map[string]interface{}{"user": aliceEmail}, "", card("Gina", "gina@other.example", "", "")),
			message("ADD", map[string]interface{}{"user": aliceEmail}, alicePath, nil),
			message("MOVE", map[string]interface{}{"user": aliceEmail}, alicePath, card("Gina", "gina@other.example", "", "")),
		} {
			raw, ok := body.(string)
			if !ok {
				b, err := json.Marshal(body)
				require.NoError(t, err)
				raw = string(b)
			}
			require.Error(t, rabbitmq.NewCommonContactsHandler().
				Handle(context.Background(), amqp.Delivery{Body: []byte(raw)}))
		}
		require.Empty(t, byPath(t, alice, alicePath))
	})
}
