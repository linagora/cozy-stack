package contact

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearch(t *testing.T) {
	config.UseTestFile(t)
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/people/search", r.URL.Path)
		assert.Equal(t, "Bearer s3cr3t", r.Header.Get("Authorization"))
		var body map[string]interface{}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, map[string]interface{}{
			"user":        "jean@acme.example",
			"q":           "dup",
			"objectTypes": []interface{}{"contact"},
			"hasFields":   []interface{}{"emailAddresses"},
			"limit":       float64(20),
			"offset":      float64(0),
		}, body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`[
			{"id": "1", "objectType": "contact",
			 "names": [{"displayName": "Marie Dupont", "type": "default"}],
			 "emailAddresses": [{"value": "marie@acme.example", "type": "Work"}, {"value": "marie@home.example"}]},
			{"id": "2", "objectType": "contact", "names": [{"displayName": "Paul Dupuis"}]},
			{"id": "3", "objectType": "contact", "emailAddresses": [{"value": "dupre@friends.example"}]}
		]`))
	}))
	t.Cleanup(ts.Close)
	config.GetConfig().ContactsSearch = map[string]config.ContactsSearch{
		"acme": {URL: ts.URL + "/api/people/search", Token: "s3cr3t"},
	}
	inst := &instance.Instance{Domain: "jean.acme.example", ContextName: "acme", Email: "jean@acme.example"}

	people, err := Search(inst, "dup", 20)
	require.NoError(t, err)
	assert.Equal(t, []Person{
		{Name: "Marie Dupont", Email: "marie@acme.example"},
		{Email: "dupre@friends.example"},
	}, people)

	status = http.StatusBadRequest
	_, err = Search(inst, "dup", 20)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoSearch)

	other := &instance.Instance{Domain: "jean.other.example", ContextName: "other", Email: "jean@other.example"}
	_, err = Search(other, "dup", 20)
	assert.ErrorIs(t, err, ErrNoSearch, "a context without contacts_search calls nothing")
	org := &instance.Instance{Domain: "org.acme.example", ContextName: "acme"}
	_, err = Search(org, "dup", 20)
	assert.ErrorIs(t, err, ErrNoSearch, "an instance without email calls nothing")
}

func TestFindByEmails(t *testing.T) {
	config.UseTestFile(t)
	db := prefixer.NewPrefixer(0, "contact-search", "contact-search")
	require.NoError(t, couchdb.ResetDB(db, consts.Contacts))
	t.Cleanup(func() { _ = couchdb.DeleteDB(db, consts.Contacts) })
	require.NoError(t, couchdb.DefineView(db, couchdb.ContactByEmail))

	create := func(fullname string, emails ...string) *Contact {
		c := New()
		c.M["fullname"] = fullname
		var list []interface{}
		for _, email := range emails {
			list = append(list, map[string]interface{}{"address": email})
		}
		if list != nil {
			c.M["email"] = list
		}
		require.NoError(t, couchdb.CreateDoc(db, c))
		return c
	}
	jean := create("Jean Dupont", "jean@acme.example")
	marie := create("Marie", "m.dupont@acme.example", "marie@home.example")
	create("Paul", "paul@acme.example")

	found, err := FindByEmails(db, []string{"jean@acme.example", "marie@home.example", "nobody@acme.example"})
	require.NoError(t, err)
	assert.Len(t, found, 2)
	assert.Equal(t, jean.ID(), found["jean@acme.example"].ID())
	assert.Equal(t, marie.ID(), found["marie@home.example"].ID())
}
