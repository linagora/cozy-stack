package job

import (
	"encoding/json"
	"testing"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShareGroupTrigger(t *testing.T) {
	trigger := &ShareGroupTrigger{}

	t.Run("The group name or color changes", func(t *testing.T) {
		group := &couchdb.JSONDoc{
			Type: consts.Groups,
			M: map[string]interface{}{
				"_id":  "id-friends",
				"_rev": "1-abcdef",
				"name": "Friends",
			},
		}

		unrelatedUpdate := group.Clone().(*couchdb.JSONDoc)
		unrelatedUpdate.M["description"] = "Friends from school"
		unrelatedUpdate.M["_rev"] = "2-abcdef"
		msg := trigger.match(&realtime.Event{
			Doc:    unrelatedUpdate,
			OldDoc: group,
			Verb:   realtime.EventUpdate,
		})
		require.Nil(t, msg)

		colored := unrelatedUpdate.Clone().(*couchdb.JSONDoc)
		colored.M["color"] = "#22AA55"
		colored.M["_rev"] = "3-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    colored,
			OldDoc: unrelatedUpdate,
			Verb:   realtime.EventUpdate,
		})
		require.NotNil(t, msg)
		require.Same(t, colored, msg.RenamedGroup)

		withoutColor := colored.Clone().(*couchdb.JSONDoc)
		delete(withoutColor.M, "color")
		withoutColor.M["_rev"] = "4-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    withoutColor,
			OldDoc: colored,
			Verb:   realtime.EventUpdate,
		})
		require.NotNil(t, msg)

		renamed := withoutColor.Clone().(*couchdb.JSONDoc)
		renamed.M["name"] = "Best friends"
		renamed.M["_rev"] = "5-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    renamed,
			OldDoc: withoutColor,
			Verb:   realtime.EventUpdate,
		})
		require.NotNil(t, msg)
	})

	t.Run("The RabbitMQ group update has a typed old document", func(t *testing.T) {
		oldGroup := contact.NewGroup()
		oldGroup.SetID("id-marketing")
		oldGroup.SetRev("1-abcdef")
		oldGroup.Type = consts.Groups
		oldGroup.M["name"] = "Marketing"
		oldGroup.M["color"] = "#3367D6"

		updated := oldGroup.JSONDoc.Clone().(*couchdb.JSONDoc)
		updated.SetRev("2-abcdef")
		updated.M["color"] = "#A142F4"

		msg := trigger.match(&realtime.Event{
			Doc:    updated,
			OldDoc: oldGroup,
			Verb:   realtime.EventUpdate,
		})

		require.NotNil(t, msg)
		require.Same(t, updated, msg.RenamedGroup)
	})

	t.Run("The contact becomes invitable", func(t *testing.T) {
		justName := &couchdb.JSONDoc{
			Type: consts.Contacts,
			M: map[string]interface{}{
				"_id":      "85507320-b157-013c-12d8-18c04daba325",
				"_rev":     "1-abcdef",
				"fullname": "Bob",
			},
		}
		msg := trigger.match(&realtime.Event{
			Doc:    justName,
			OldDoc: nil,
			Verb:   realtime.EventCreate,
		})
		require.Nil(t, msg)

		withAnEmail := justName.Clone().(*couchdb.JSONDoc)
		withAnEmail.M["email"] = []interface{}{
			map[string]interface{}{
				"address": "bob@example.net",
			},
		}
		withAnEmail.M["_rev"] = "2-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    withAnEmail,
			OldDoc: justName,
			Verb:   realtime.EventUpdate,
		})
		require.NotNil(t, msg)
		assert.Equal(t, msg.ContactID, "85507320-b157-013c-12d8-18c04daba325")
		assert.Len(t, msg.GroupsAdded, 0)
		assert.Len(t, msg.GroupsRemoved, 0)
		assert.True(t, msg.BecomeInvitable)

		withCozyURL := justName.Clone().(*couchdb.JSONDoc)
		withCozyURL.M["cozy"] = []interface{}{
			map[string]interface{}{
				"url": "bob.mycozy.cloud",
			},
		}
		withCozyURL.M["_rev"] = "2-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    withCozyURL,
			OldDoc: justName,
			Verb:   realtime.EventUpdate,
		})
		require.NotNil(t, msg)
		assert.Equal(t, msg.ContactID, "85507320-b157-013c-12d8-18c04daba325")
		assert.Len(t, msg.GroupsAdded, 0)
		assert.Len(t, msg.GroupsRemoved, 0)
		assert.True(t, msg.BecomeInvitable)

		both := withAnEmail.Clone().(*couchdb.JSONDoc)
		both.M["cozy"] = []interface{}{
			map[string]interface{}{
				"url": "bob.mycozy.cloud",
			},
		}
		both.M["_rev"] = "3-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    both,
			OldDoc: withAnEmail,
			Verb:   realtime.EventUpdate,
		})
		assert.Nil(t, msg)
	})

	t.Run("Groups are added/removed to a contact", func(t *testing.T) {
		noGroup := &couchdb.JSONDoc{
			Type: consts.Contacts,
			M: map[string]interface{}{
				"_id":      "85507320-b157-013c-12d8-18c04daba326",
				"_rev":     "1-abcdef",
				"fullname": "Bob",
			},
		}
		msg := trigger.match(&realtime.Event{
			Doc:    noGroup,
			OldDoc: nil,
			Verb:   realtime.EventCreate,
		})
		require.Nil(t, msg)

		updatedName := noGroup.Clone().(*couchdb.JSONDoc)
		updatedName.M["fullname"] = "Bobby"
		updatedName.M["_rev"] = "2-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    updatedName,
			OldDoc: noGroup,
			Verb:   realtime.EventUpdate,
		})
		require.Nil(t, msg)

		var groups map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(`{
    "groups": {
      "data": [
        {
          "_id": "id-friends",
          "_type": "io.cozy.contacts.groups"
        },
        {
          "_id": "id-football",
          "_type": "io.cozy.contacts.groups"
        }
      ]
    }
}`), &groups))
		addedInGroups := updatedName.Clone().(*couchdb.JSONDoc)
		addedInGroups.M["relationships"] = groups
		addedInGroups.M["_rev"] = "3-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    addedInGroups,
			OldDoc: updatedName,
			Verb:   realtime.EventUpdate,
		})
		require.NotNil(t, msg)
		assert.Equal(t, msg.ContactID, "85507320-b157-013c-12d8-18c04daba326")
		assert.EqualValues(t, msg.GroupsAdded, []string{"id-friends", "id-football"})
		assert.Len(t, msg.GroupsRemoved, 0)
		assert.False(t, msg.BecomeInvitable)

		var groups2 map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(`{
    "groups": {
      "data": [
        {
          "_id": "id-friends",
          "_type": "io.cozy.contacts.groups"
        }
      ]
    }
}`), &groups2))
		removedFromFootball := addedInGroups.Clone().(*couchdb.JSONDoc)
		removedFromFootball.M["relationships"] = groups2
		removedFromFootball.M["_rev"] = "4-abcdef"
		msg = trigger.match(&realtime.Event{
			Doc:    removedFromFootball,
			OldDoc: addedInGroups,
			Verb:   realtime.EventUpdate,
		})
		require.NotNil(t, msg)
		assert.Equal(t, msg.ContactID, "85507320-b157-013c-12d8-18c04daba326")
		assert.Len(t, msg.GroupsAdded, 0)
		assert.EqualValues(t, msg.GroupsRemoved, []string{"id-football"})
		assert.False(t, msg.BecomeInvitable)

		deleted := &couchdb.JSONDoc{
			Type: consts.Contacts,
			M: map[string]interface{}{
				"_id":      removedFromFootball.ID(),
				"_rev":     "5-abcdef",
				"_deleted": true,
			},
		}
		msg = trigger.match(&realtime.Event{
			Doc:    deleted,
			OldDoc: removedFromFootball,
			Verb:   realtime.EventDelete,
		})
		require.NotNil(t, msg)
		assert.Equal(t, msg.ContactID, "85507320-b157-013c-12d8-18c04daba326")
		assert.Len(t, msg.GroupsAdded, 0)
		assert.EqualValues(t, msg.GroupsRemoved, []string{"id-friends"})
		assert.False(t, msg.BecomeInvitable)
	})
}
