package contact

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindAllByEmail(t *testing.T) {
	config.UseTestFile(t)
	instPrefix := prefixer.NewPrefixer(0, "contact-by-email", "contact-by-email")
	require.NoError(t, couchdb.ResetDB(instPrefix, consts.Contacts))
	t.Cleanup(func() { _ = couchdb.DeleteDB(instPrefix, consts.Contacts) })
	require.NoError(t, couchdb.DefineView(instPrefix, couchdb.ContactByEmail))

	external := New()
	external.M["fullname"] = "Alice"
	external.M["email"] = []map[string]interface{}{
		{"address": "alice@example.com", "primary": true},
	}
	external.M["metadata"] = map[string]interface{}{"external": true}
	require.NoError(t, couchdb.CreateDoc(instPrefix, external))

	other := New()
	other.M["fullname"] = "Bob"
	other.M["email"] = []map[string]interface{}{
		{"address": "alice@example.com", "primary": true},
	}
	require.NoError(t, couchdb.CreateDoc(instPrefix, other))

	docs, err := FindAllByEmail(instPrefix, "alice@example.com")
	require.NoError(t, err)
	require.Len(t, docs, 2)
	ids := []string{docs[0].ID(), docs[1].ID()}
	assert.Contains(t, ids, external.ID())
	assert.True(t, docs[0].IsExternal() || docs[1].IsExternal())

	_, err = FindAllByEmail(instPrefix, "missing@example.com")
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestCreate(t *testing.T) {
	config.UseTestFile(t)
	instPrefix := prefixer.NewPrefixer(0, "contact-create", "contact-create")
	require.NoError(t, couchdb.ResetDB(instPrefix, consts.Contacts))
	t.Cleanup(func() { _ = couchdb.DeleteDB(instPrefix, consts.Contacts) })
	require.NoError(t, couchdb.DefineView(instPrefix, couchdb.ContactByEmail))

	t.Run("WithAllFields", func(t *testing.T) {
		doc, err := Create(instPrefix, CreateOptions{
			Email:    "alice@example.com",
			Name:     "Alice Doe",
			CozyURL:  "https://alice.example",
			Phone:    "+33123456789",
			External: true,
		})
		require.NoError(t, err)
		// The returned doc must be readable without a round trip to CouchDB
		created, err := doc.ToMailAddress()
		require.NoError(t, err)
		require.Equal(t, "alice@example.com", created.Email)
		require.Equal(t, "https://alice.example", doc.PrimaryCozyURL())
		require.Equal(t, "+33123456789", doc.PrimaryPhoneNumber())

		stored, err := Find(instPrefix, doc.ID())
		require.NoError(t, err)
		require.Equal(t, "Alice Doe", stored.PrimaryName())
		require.Equal(t, "https://alice.example", stored.PrimaryCozyURL())
		require.Equal(t, "+33123456789", stored.PrimaryPhoneNumber())
		require.True(t, stored.IsExternal())
		addr, err := stored.ToMailAddress()
		require.NoError(t, err)
		require.Equal(t, "alice@example.com", addr.Email)

		indexes, ok := stored.M["indexes"].(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, "https://alice.example", indexes["byFamilyNameGivenNameEmailCozyUrl"])
	})

	t.Run("FallbackNameUsesEmailLocalPart", func(t *testing.T) {
		doc, err := Create(instPrefix, CreateOptions{
			Email: "bob@example.com",
		})
		require.NoError(t, err)

		stored, err := Find(instPrefix, doc.ID())
		require.NoError(t, err)
		require.Equal(t, "bob", stored.PrimaryName())
		displayName, _ := stored.M["displayName"].(string)
		require.Equal(t, "bob@example.com", displayName)
	})

	t.Run("MissingEmail", func(t *testing.T) {
		_, err := Create(instPrefix, CreateOptions{})
		require.ErrorIs(t, err, ErrNoMailAddress)
	})
}

func TestGetAllContacts(t *testing.T) {
	config.UseTestFile(t)
	instPrefix := prefixer.NewPrefixer(0, "cozy-test", "cozy-test")
	t.Cleanup(func() { _ = couchdb.DeleteDB(instPrefix, consts.Contacts) })

	g := NewGroup()
	g.SetID(uuid.Must(uuid.NewV7()).String())

	gaby := fmt.Sprintf(`{
  "address": [],
  "birthday": "",
  "birthplace": "",
  "company": "",
  "cozy": [],
  "cozyMetadata": {
    "createdAt": "2024-02-13T15:05:58.917Z",
    "createdByApp": "Contacts",
    "createdByAppVersion": "1.7.0",
    "doctypeVersion": 3,
    "metadataVersion": 1,
    "updatedAt": "2024-02-13T15:06:21.046Z",
    "updatedByApps": [
      {
        "date": "2024-02-13T15:06:21.046Z",
        "slug": "Contacts",
        "version": "1.7.0"
      }
    ]
  },
  "displayName": "Gaby",
  "email": [],
  "fullname": "Gaby",
  "gender": "female",
  "indexes": {
    "byFamilyNameGivenNameEmailCozyUrl": "gaby"
  },
  "jobTitle": "",
  "metadata": {
    "cozy": true,
    "version": 1
  },
  "name": {
    "givenName": "Gaby"
  },
  "note": "",
  "phone": [],
  "relationships": {
    "groups": {
      "data": [
        {
          "_id": "%s",
          "_type": "io.cozy.contacts.groups"
        }
      ]
    }
  }
}`, g.ID())

	doc := couchdb.JSONDoc{Type: consts.Contacts, M: make(map[string]interface{})}
	require.NoError(t, json.Unmarshal([]byte(gaby), &doc.M))
	require.NoError(t, couchdb.CreateDoc(instPrefix, &doc))

	contacts, err := g.GetAllContacts(instPrefix)
	require.NoError(t, err)
	require.Len(t, contacts, 1)
	assert.Equal(t, contacts[0].PrimaryName(), "Gaby")
}
