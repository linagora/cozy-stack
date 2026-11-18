package sharing_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	. "github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Importing worker/share would also run the group and replication workers,
// which race with the assertions of the other sharing tests.
func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:  CollectedContactsWorker,
		Concurrency: 1,
		WorkerFunc:  func(*job.TaskContext) error { return nil },
	})
}

func TestPublishPersonalContacts(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	setup.GetTestInstance()

	config.GetConfig().Contexts = map[string]interface{}{
		"personal-on": map[string]interface{}{"common_contacts": true},
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	newInstance := func(name, contextName, email string) *instance.Instance {
		inst, err := lifecycle.Create(&lifecycle.Options{Domain: name + "-" + suffix + ".pc.localhost", ContextName: contextName})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
		if email != "" {
			require.NoError(t, instance.SetEmail(inst, email))
		}
		return inst
	}
	newContact := func(inst *instance.Instance, fields map[string]interface{}) *couchdb.JSONDoc {
		doc := &couchdb.JSONDoc{Type: consts.Contacts, M: fields}
		require.NoError(t, couchdb.CreateDoc(inst, doc))
		return doc
	}
	email := func(addresses ...string) []interface{} {
		list := make([]interface{}, len(addresses))
		for i, a := range addresses {
			list[i] = map[string]interface{}{"address": a, "primary": i == 0}
		}
		return list
	}

	alice := newInstance("alice", "personal-on", "alice-"+suffix+"@acme.example")
	typed := newContact(alice, map[string]interface{}{
		"fullname": "Bob Martin",
		"name":     map[string]interface{}{"givenName": "Bob", "familyName": "Martin"},
		"email":    email("dave@unknown.example", "[REDACTED-EMAIL-f2c9bdb2]"),
		"phone":    []interface{}{map[string]interface{}{"number": "+33600000000", "primary": true}},
		"address":  []interface{}{map[string]interface{}{"city": "Paris"}},
	})
	shared, err := contact.Create(alice, contact.CreateOptions{Email: "erin@unknown.example", External: true})
	require.NoError(t, err)
	dupOfShared := newContact(alice, map[string]interface{}{"email": email("erin@unknown.example")})
	dupOfFeed := newContact(alice, map[string]interface{}{"email": email("[REDACTED-EMAIL-ad1a5d51]")})
	newContact(alice, map[string]interface{}{"me": true, "email": email("[REDACTED-EMAIL-07b0be32]")})
	newContact(alice, map[string]interface{}{"trashed": true, "email": email("[REDACTED-EMAIL-ff21cf4f]")})
	newContact(alice, map[string]interface{}{"phone": []interface{}{map[string]interface{}{"number": "+33611111111"}}})
	newContact(alice, map[string]interface{}{contact.CardDAVPathKey: "/addressbooks/a/collected/x.vcf", "email": email("[REDACTED-EMAIL-ad1a5d51]")})
	newContact(alice, map[string]interface{}{
		"twakeDirectory": map[string]interface{}{"managed": true, "organizationId": "org-" + suffix},
		"email":          email("[REDACTED-EMAIL-be7a6c38]"),
	})
	standalone := newInstance("bob", "", "bob-"+suffix+"@acme.example")
	newContact(standalone, map[string]interface{}{"email": email("[REDACTED-EMAIL-ea04f83d]")})
	noEmail := newInstance("carol", "personal-on", "")
	newContact(noEmail, map[string]interface{}{"email": email("[REDACTED-EMAIL-ea04f83d]")})
	insts := []*instance.Instance{alice, standalone, noEmail}

	collectedJobs := func(inst *instance.Instance) []CollectedContactsMsg {
		jobs, err := job.GetAllJobs(inst)
		require.NoError(t, err)
		var msgs []CollectedContactsMsg
		for _, j := range jobs {
			if j.WorkerType != CollectedContactsWorker {
				continue
			}
			var msg CollectedContactsMsg
			require.NoError(t, json.Unmarshal(j.Message, &msg))
			msgs = append(msgs, msg)
		}
		return msgs
	}

	t.Run("a dry run counts the personal contacts and changes nothing", func(t *testing.T) {
		report := PublishPersonalContacts(insts, true)
		assert.Equal(t, 3, report.Scanned)
		assert.Equal(t, 2, report.Skipped)
		assert.Equal(t, 2, report.Published)
		assert.Empty(t, report.Errors)

		assert.Empty(t, collectedJobs(alice))
		var doc couchdb.JSONDoc
		require.NoError(t, couchdb.GetDoc(alice, consts.Contacts, typed.ID(), &doc))
		assert.NotContains(t, doc.M, "metadata")
	})

	t.Run("publishing queues the full contacts and marks them external", func(t *testing.T) {
		report := PublishPersonalContacts(insts, false)
		assert.Equal(t, 2, report.Published)
		assert.Empty(t, report.Errors)

		msgs := collectedJobs(alice)
		require.Len(t, msgs, 1)
		assert.Empty(t, msgs[0].Emails)
		assert.ElementsMatch(t, []CollectedContact{
			{Name: "Bob Martin", GivenName: "Bob", FamilyName: "Martin", Emails: []string{"dave@unknown.example", "[REDACTED-EMAIL-f2c9bdb2]"}, Phones: []string{"+33600000000"}},
			{Name: "erin", Emails: []string{"erin@unknown.example"}},
		}, msgs[0].Contacts)

		// A contact whose email already has an external contact or a card is
		// left as it is, so that each email keeps a single external contact.
		for _, dup := range []*couchdb.JSONDoc{dupOfShared, dupOfFeed} {
			var doc couchdb.JSONDoc
			require.NoError(t, couchdb.GetDoc(alice, consts.Contacts, dup.ID(), &doc))
			assert.NotContains(t, doc.M, "metadata")
		}
		assert.Empty(t, collectedJobs(standalone))
		assert.Empty(t, collectedJobs(noEmail))

		// The card coming back from Sabre attaches to the same contact.
		found, err := contact.FindExternalWithoutCardDAVPath(alice, "dave@unknown.example")
		require.NoError(t, err)
		assert.Equal(t, typed.ID(), found.ID())
		assert.Equal(t, []interface{}{map[string]interface{}{"city": "Paris"}}, found.Get("address"))
		found, err = contact.FindExternalWithoutCardDAVPath(alice, "erin@unknown.example")
		require.NoError(t, err)
		assert.Equal(t, shared.ID(), found.ID())
	})
}
