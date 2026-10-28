package sharing

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSuggest(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	conf := config.GetConfig()
	previous := conf.Contexts
	centralizedContext := map[string]interface{}{"centralized_org_directory": true, "common_contacts": true}
	conf.Contexts = map[string]interface{}{"suggest": centralizedContext}
	t.Cleanup(func() { conf.Contexts = previous })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "fe" + suffix
	orgDomain := "acme-" + suffix + ".example"
	newInstance := func(domain, email string) *instance.Instance {
		inst, err := lifecycle.Create(&lifecycle.Options{
			Domain: domain, OrgDomain: orgDomain, OrgID: orgID, ContextName: "suggest", Email: email,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
		return inst
	}
	org := newInstance(orgID+".fe.localhost", "")
	alice := newInstance("alice-"+suffix+".fe.localhost", "alice@acme.example")
	alice.Email = "alice@acme.example"

	create := func(inst *instance.Instance, name, email string) *contact.Contact {
		c := contact.New()
		// Stable IDs put the self match before Bob for the limit regression.
		c.SetID(name + "-" + inst.Domain)
		c.M["fullname"] = name
		c.M["email"] = []interface{}{map[string]interface{}{"address": email, "primary": true}}
		require.NoError(t, couchdb.CreateNamedDocWithDB(inst, c))
		return c
	}
	create(org, "Alice Durand", "alice@acme.example")
	bob := create(org, "Bob Durand", "bob@acme.example")
	create(alice, "Alice Durand", "alice@acme.example")
	localBob := create(alice, "Bob Durand", "bob@acme.example") // a member copy
	carol := create(alice, "Carol Durand", "carol@friends.example")

	var calls atomic.Int32
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Limit int `json:"limit"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		w.WriteHeader(status)
		people := []json.RawMessage{
			json.RawMessage(`{"names": [{"displayName": "Alice Durand"}], "emailAddresses": [{"value": "alice@acme.example"}]}`),
			json.RawMessage(`{"names": [{"displayName": "Bob Durand"}], "emailAddresses": [{"value": "bob@acme.example"}]}`),
			json.RawMessage(`{"names": [{"displayName": "Carol Durand"}], "emailAddresses": [{"value": "Carol@Friends.example"}]}`),
			json.RawMessage(`{"names": [{"displayName": "Dave Durand"}], "emailAddresses": [{"value": "dave@unknown.example"}]}`),
			json.RawMessage(`{"names": [{"displayName": "Bob"}], "emailAddresses": [{"value": "BOB@acme.example"}]}`),
		}
		assert.NoError(t, json.NewEncoder(w).Encode(people[:min(request.Limit, len(people))]))
	}))
	t.Cleanup(ts.Close)
	config.GetConfig().ContactsSearch = map[string]config.ContactsSearch{
		"suggest": {URL: ts.URL, Token: "s3cr3t"},
	}
	t.Cleanup(func() { config.GetConfig().ContactsSearch = nil })

	type result struct{ id, kind, name, email string }
	results := func(suggestions []Suggestion) []result {
		var list []result
		for _, s := range suggestions {
			r := result{kind: s.Kind, name: s.Name, email: s.Email}
			if s.Contact != nil {
				r.id = s.Contact.ID()
			}
			list = append(list, r)
		}
		return list
	}

	t.Run("FromTheSideService", func(t *testing.T) {
		suggestions, err := Suggest(alice, "duran", 20)
		require.NoError(t, err)
		assert.Equal(t, []result{
			{bob.ID(), SuggestionMember, "Bob Durand", "bob@acme.example"},
			{carol.ID(), SuggestionContact, "Carol Durand", "Carol@Friends.example"},
			{"", "", "Dave Durand", "dave@unknown.example"},
		}, results(suggestions))
	})

	for _, centralized := range []bool{false, true} {
		t.Run(fmt.Sprintf("LocalWithoutService/centralized=%t", centralized), func(t *testing.T) {
			conf.Contexts["suggest"] = map[string]interface{}{"centralized_org_directory": centralized}
			t.Cleanup(func() { conf.Contexts["suggest"] = centralizedContext })
			before := calls.Load()
			suggestions, err := Suggest(alice, "DURAN", 20)
			require.NoError(t, err)
			want := []result{
				{bob.ID(), SuggestionMember, "Bob Durand", "bob@acme.example"},
				{carol.ID(), SuggestionContact, "Carol Durand", "carol@friends.example"},
			}
			if !centralized {
				want[0].id, want[0].kind = localBob.ID(), SuggestionContact
			}
			assert.Equal(t, want, results(suggestions))
			assert.Equal(t, before, calls.Load(), "local lookup must not call the external service")
		})
	}

	t.Run("FallsBackToTheLocalSearch", func(t *testing.T) {
		status = http.StatusServiceUnavailable
		t.Cleanup(func() { status = http.StatusOK })
		suggestions, err := Suggest(alice, "DURAN", 20)
		require.NoError(t, err)
		assert.Equal(t, []result{
			{bob.ID(), SuggestionMember, "Bob Durand", "bob@acme.example"},
			{carol.ID(), SuggestionContact, "Carol Durand", "carol@friends.example"},
		}, results(suggestions))
	})
	t.Run("GroupLimitKeepsPeopleAfterFilteringSelf", func(t *testing.T) {
		t.Cleanup(func() { conf.Contexts["suggest"] = centralizedContext })
		for _, db := range []*instance.Instance{org, alice} {
			group := contact.NewGroup()
			group.SetID(orgdirectory.GroupDocID(orgID, "limit"))
			group.M["name"] = "Durand Team"
			group.M[orgdirectory.DirectoryMetadataKey] = map[string]interface{}{"managed": true, "organizationId": orgID}
			require.NoError(t, couchdb.CreateNamedDocWithDB(db, group))
			t.Cleanup(func() { require.NoError(t, couchdb.DeleteDoc(db, group)) })
		}
		for _, centralized := range []bool{false, true} {
			for _, common := range []bool{false, true} {
				t.Run(fmt.Sprintf("centralized=%t/common=%t", centralized, common), func(t *testing.T) {
					conf.Contexts["suggest"] = map[string]interface{}{
						"centralized_org_directory": centralized, "common_contacts": common,
					}
					suggestions, err := Suggest(alice, "du", 2)
					require.NoError(t, err)
					require.Len(t, suggestions, 2)
					assert.NotNil(t, suggestions[0].Group)
					assert.Equal(t, "bob@acme.example", suggestions[1].Email)
				})
			}
		}
	})
	t.Run("OrganizationGroups", func(t *testing.T) {
		makeGroup := func(db *instance.Instance, id, name, owner string, managed bool) *contact.Group {
			g := contact.NewGroup()
			g.SetID(id)
			g.M["name"] = name
			g.M["color"] = "#123456"
			g.M[orgdirectory.DirectoryMetadataKey] = map[string]interface{}{
				"managed": managed, "organizationId": owner,
			}
			require.NoError(t, couchdb.CreateNamedDocWithDB(db, g))
			return g
		}
		team := makeGroup(org, orgdirectory.GroupDocID(orgID, "team"), "Durand Team", orgID, true)
		other := makeGroup(org, orgdirectory.GroupDocID(orgID, "other"), "Durand Other", orgID, true)
		localTeam := makeGroup(alice, team.ID(), "Durand stale", orgID, true)
		personal := makeGroup(alice, "personal-group", "Durand Friends", "", false)
		create(org, "Olivia Durand", "olivia@acme.example")
		makeGroup(org, orgdirectory.GroupDocID("another", "team"), "Durand Foreign", "another", true)
		makeGroup(org, orgdirectory.GroupDocID(orgID, "unmanaged"), "Durand Unmanaged", orgID, false)
		makeGroup(org, "personal-group", "Durand Personal", orgID, true)
		makeGroup(org, orgdirectory.GroupDocID(orgID, "special"), "Dev [Ops]", orgID, true)
		createContactInGroups(t, org, "Engineer", []string{other.ID()})

		for _, centralized := range []bool{false, true} {
			for _, common := range []bool{false, true} {
				t.Run(fmt.Sprintf("centralized=%t/common=%t", centralized, common), func(t *testing.T) {
					conf.Contexts["suggest"] = map[string]interface{}{
						"centralized_org_directory": centralized, "common_contacts": common,
					}
					before := calls.Load()
					suggestions, err := Suggest(alice, "DU", 20)
					require.NoError(t, err)
					require.GreaterOrEqual(t, len(suggestions), 4)
					require.NotNil(t, suggestions[0].Group)
					require.NotNil(t, suggestions[1].Group)
					wantGroups := []string{team.Name(), other.Name()}
					if !centralized {
						wantGroups = []string{localTeam.Name(), personal.Name()}
						assert.Equal(t, []result{
							{localBob.ID(), SuggestionContact, "Bob Durand", "bob@acme.example"},
							{carol.ID(), SuggestionContact, "Carol Durand", "carol@friends.example"},
						}, results(suggestions[2:]))
						assert.Equal(t, before, calls.Load(), "local-only mode must not call the external service")
					}
					assert.ElementsMatch(t, wantGroups, []string{suggestions[0].Group.Name(), suggestions[1].Group.Name()})
					for _, suggestion := range suggestions[:2] {
						count := 0
						if centralized && suggestion.Group.ID() == other.ID() {
							count = 1
						}
						assert.Equal(t, count, suggestion.MemberCount)
					}
					assert.Equal(t, "#123456", suggestions[0].Group.Color())
					assert.Nil(t, suggestions[2].Group)
					limited, err := Suggest(alice, "du", 1)
					require.NoError(t, err)
					require.Len(t, limited, 1)
					assert.NotNil(t, limited[0].Group)
				})
			}
		}
		conf.Contexts["suggest"] = centralizedContext
		status = http.StatusServiceUnavailable
		t.Cleanup(func() { status = http.StatusOK })
		suggestions, err := Suggest(alice, "dur", 20)
		require.NoError(t, err)
		require.Len(t, suggestions, 5)
		assert.NotNil(t, suggestions[0].Group)
		assert.NotNil(t, suggestions[1].Group)
		assert.Equal(t, bob.ID(), suggestions[2].Contact.ID())

		literal, err := Suggest(alice, "[O", 20)
		require.NoError(t, err)
		require.NotEmpty(t, literal)
		assert.Equal(t, "Dev [Ops]", literal[0].Group.Name())

		conf.Contexts["suggest"] = map[string]interface{}{"centralized_org_directory": true}
		sharing := &Sharing{Active: true, Owner: true, Members: []Member{{Status: MemberStatusOwner}}}
		require.NoError(t, sharing.AddGroup(alice, other.ID(), false))
		require.Len(t, sharing.Groups, 1)
		assert.Equal(t, other.ID(), sharing.Groups[0].ID)
		require.Len(t, sharing.Members, 2)
		assert.Equal(t, "engineer@cozy.tools", sharing.Members[1].Email)
	})
}
