package sharing

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
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
		c, err := contact.Create(inst, contact.CreateOptions{Name: name, Email: email})
		require.NoError(t, err)
		return c
	}
	create(org, "Alice Durand", "alice@acme.example")
	bob := create(org, "Bob Durand", "bob@acme.example")
	create(alice, "Bob Durand", "bob@acme.example") // a member copy
	carol := create(alice, "Carol Durand", "carol@friends.example")

	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`[
			{"names": [{"displayName": "Alice Durand"}], "emailAddresses": [{"value": "alice@acme.example"}]},
			{"names": [{"displayName": "Bob Durand"}], "emailAddresses": [{"value": "bob@acme.example"}]},
			{"names": [{"displayName": "Carol Durand"}], "emailAddresses": [{"value": "carol@friends.example"}]},
			{"names": [{"displayName": "Dave Durand"}], "emailAddresses": [{"value": "dave@unknown.example"}]},
			{"names": [{"displayName": "Bob"}], "emailAddresses": [{"value": "BOB@acme.example"}]}
		]`))
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
			{carol.ID(), SuggestionContact, "Carol Durand", "carol@friends.example"},
			{"", "", "Dave Durand", "dave@unknown.example"},
		}, results(suggestions))
	})

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
}
