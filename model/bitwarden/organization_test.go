package bitwarden

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/stretchr/testify/assert"
)

func TestOrganizationMember(t *testing.T) {
	org := &Organization{
		Members: map[string]OrgMember{
			"alice.mycozy.cloud": {
				UserID: "alice",
				OrgKey: "alice-key",
				Status: OrgMemberConfirmed,
				Owner:  true,
			},
			"bob.twake.app": {
				UserID: "bob",
				OrgKey: "bob-key",
				Status: OrgMemberConfirmed,
			},
		},
	}

	t.Run("the member is found by the domain", func(t *testing.T) {
		inst := &instance.Instance{Domain: "bob.twake.app"}
		m := org.Member(inst)
		assert.Equal(t, "bob", m.UserID)
		assert.Equal(t, "bob-key", m.OrgKey)
	})

	t.Run("the member is found by the old domain", func(t *testing.T) {
		inst := &instance.Instance{
			Domain:    "alice.twake.app",
			OldDomain: "alice.mycozy.cloud",
		}
		m := org.Member(inst)
		assert.Equal(t, "alice", m.UserID)
		assert.Equal(t, "alice-key", m.OrgKey)
		assert.True(t, m.Owner)
	})

	t.Run("the domain has the precedence over the old domain", func(t *testing.T) {
		inst := &instance.Instance{
			Domain:    "bob.twake.app",
			OldDomain: "alice.mycozy.cloud",
		}
		m := org.Member(inst)
		assert.Equal(t, "bob", m.UserID)
	})

	t.Run("the member is found by a domain alias", func(t *testing.T) {
		inst := &instance.Instance{
			Domain:        "alice.twake.app",
			DomainAliases: []string{"other.example.org", "alice.mycozy.cloud"},
		}
		m := org.Member(inst)
		assert.Equal(t, "alice", m.UserID)
	})

	t.Run("an entry with a key has the precedence over a keyless entry", func(t *testing.T) {
		migrated := &Organization{
			Members: map[string]OrgMember{
				"alice.mycozy.cloud": {
					UserID: "alice",
					OrgKey: "alice-key",
					Status: OrgMemberConfirmed,
				},
				"alice.twake.app": {
					UserID: "alice",
					Status: OrgMemberAccepted,
				},
			},
		}
		inst := &instance.Instance{
			Domain:    "alice.twake.app",
			OldDomain: "alice.mycozy.cloud",
		}
		m := migrated.Member(inst)
		assert.Equal(t, "alice-key", m.OrgKey)
		assert.Equal(t, OrgMemberConfirmed, m.Status)
	})

	t.Run("the keyless entry is returned when no entry has a key", func(t *testing.T) {
		pending := &Organization{
			Members: map[string]OrgMember{
				"alice.twake.app": {
					UserID: "alice",
					Status: OrgMemberAccepted,
				},
			},
		}
		inst := &instance.Instance{
			Domain:    "alice.twake.app",
			OldDomain: "alice.mycozy.cloud",
		}
		m := pending.Member(inst)
		assert.Equal(t, "alice", m.UserID)
		assert.Equal(t, OrgMemberAccepted, m.Status)
	})

	t.Run("a zero value is returned for a non member", func(t *testing.T) {
		inst := &instance.Instance{
			Domain:    "charlie.twake.app",
			OldDomain: "charlie.mycozy.cloud",
		}
		m := org.Member(inst)
		assert.Empty(t, m.UserID)
		assert.Empty(t, m.OrgKey)
		assert.False(t, m.Owner)
		assert.Equal(t, OrgMemberInvited, m.Status)
	})
}
