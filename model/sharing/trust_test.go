package sharing

import (
	"testing"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestIsTrustedMember(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	cfg := config.GetConfig()
	prevContexts := cfg.Contexts
	// Initialize contexts with sharing config
	cfg.Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted": true,
			},
		},
	}
	t.Cleanup(func() { cfg.Contexts = prevContexts })

	inst.Domain = "owner.example.com"

	t.Run("no instance URL", func(t *testing.T) {
		member := &Member{Email: "outsider@unknown.net"}
		require.False(t, IsTrustedMember(inst, member))
	})

	t.Run("untrusted instance domain", func(t *testing.T) {
		member := &Member{Email: "outsider@unknown.net", Instance: "https://stranger.unknown.net"}
		require.False(t, IsTrustedMember(inst, member))
	})

	t.Run("Scenario 1: B2C SaaS (twake.app) - users DON'T trust each other", func(t *testing.T) {
		// In B2C SaaS, users like alice.twake.app and bob.twake.app should NOT trust each other
		// Configuration: Don't add twake.app to TrustedDomains
		prevDefault := cfg.Contexts[config.DefaultInstanceContext]
		cfg.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted": true,
				"trusted_domains":     []interface{}{}, // Empty - no trust between users
			},
		}
		t.Cleanup(func() { cfg.Contexts[config.DefaultInstanceContext] = prevDefault })

		inst.Domain = "alice.twake.app"
		member := &Member{Email: "bob@twake.app", Instance: "https://bob.twake.app"}

		// Alice should NOT trust Bob (no trusted domains configured)
		require.False(t, IsTrustedMember(inst, member))
	})

	t.Run("Scenario 2: B2B SaaS org - users within org trust each other", func(t *testing.T) {
		// In B2B SaaS, users within the same organization should trust each other
		// Organization: linagora.twake.app
		// Users: alice.linagora.twake.app, bob.linagora.twake.app
		prevDefault := cfg.Contexts[config.DefaultInstanceContext]
		cfg.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted": true,
				"trusted_domains":     []interface{}{"linagora.twake.app"},
			},
		}
		t.Cleanup(func() { cfg.Contexts[config.DefaultInstanceContext] = prevDefault })

		t.Run("users in same org trust each other", func(t *testing.T) {
			inst.Domain = "alice.linagora.twake.app"
			member := &Member{Email: "bob@linagora.com", Instance: "https://bob.linagora.twake.app"}

			// Bob's domain ends with "linagora.twake.app" → should trust
			require.True(t, IsTrustedMember(inst, member))
		})

		t.Run("users from different orgs DON'T trust each other", func(t *testing.T) {
			inst.Domain = "alice.linagora.twake.app"
			member := &Member{Email: "charlie@acme.com", Instance: "https://charlie.acme.twake.app"}

			// Charlie's domain "charlie.acme.twake.app" doesn't end with "linagora.twake.app" → should NOT trust
			require.False(t, IsTrustedMember(inst, member))
		})

		t.Run("deeper subdomain levels within same org", func(t *testing.T) {
			inst.Domain = "alice.eng.linagora.twake.app"
			member := &Member{Email: "bob@linagora.com", Instance: "https://bob.sales.linagora.twake.app"}

			// Bob's domain ends with "linagora.twake.app" → should trust
			require.True(t, IsTrustedMember(inst, member))
		})
	})

	t.Run("Scenario 3: On-premise - all users trust each other", func(t *testing.T) {
		// On-premise deployment where all users under *.linagora.com trust each other
		prevDefault := cfg.Contexts[config.DefaultInstanceContext]
		cfg.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted": true,
				"trusted_domains":     []interface{}{"linagora.com"},
			},
		}
		t.Cleanup(func() { cfg.Contexts[config.DefaultInstanceContext] = prevDefault })

		t.Run("all users under same domain trust each other", func(t *testing.T) {
			inst.Domain = "alice.linagora.com"
			member := &Member{Email: "bob@linagora.com", Instance: "https://bob.linagora.com"}

			// Bob's domain ends with "linagora.com" → should trust
			require.True(t, IsTrustedMember(inst, member))
		})

		t.Run("subdomain levels trust each other", func(t *testing.T) {
			inst.Domain = "alice.eng.linagora.com"
			member := &Member{Email: "bob@linagora.com", Instance: "https://bob.sales.linagora.com"}

			// Bob's domain ends with "linagora.com" → should trust
			require.True(t, IsTrustedMember(inst, member))
		})

		t.Run("users from different root domains DON'T trust", func(t *testing.T) {
			inst.Domain = "alice.linagora.com"
			member := &Member{Email: "eve@evil.com", Instance: "https://eve.evil.com"}

			// Eve's domain doesn't end with "linagora.com" → should NOT trust
			require.False(t, IsTrustedMember(inst, member))
		})
	})

	t.Run("Contact-based trust", func(t *testing.T) {
		// Configure with NO trusted domains

		prevDefault := cfg.Contexts[config.DefaultInstanceContext]
		cfg.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
			"sharing": map[string]interface{}{
				"auto_accept_trusted":          true,
				"trusted_domains":              []interface{}{},
				"auto_accept_trusted_contacts": true,
			},
		}
		t.Cleanup(func() { cfg.Contexts[config.DefaultInstanceContext] = prevDefault })

		t.Run("untrusted contact from untrusted domain", func(t *testing.T) {
			inst.Domain = "alice.example.com"
			member := &Member{
				Email:    "bob@other.com",
				Instance: "https://bob.other.com",
			}

			// No domain trust, no contact trust
			require.False(t, IsTrustedMember(inst, member))
		})

		t.Run("trusted contact from untrusted domain", func(t *testing.T) {
			// Create a contact and mark as trusted
			c := contact.New()
			c.M["email"] = []interface{}{
				map[string]interface{}{"address": "charlie@external.com"},
			}
			require.NoError(t, couchdb.CreateDoc(inst, c))
			require.NoError(t, c.MarkAsTrusted(inst))

			inst.Domain = "alice.example.com"
			member := &Member{
				Email:    "charlie@external.com",
				Instance: "https://charlie.external.com",
			}

			// No domain trust, but contact is trusted
			require.True(t, IsTrustedMember(inst, member))
		})

		t.Run("member without email cannot be contact-trusted", func(t *testing.T) {
			inst.Domain = "alice.example.com"
			member := &Member{
				Email:    "", // No email
				Instance: "https://someone.other.com",
			}

			require.False(t, IsTrustedMember(inst, member))
		})

		t.Run("contact not found - should not be trusted", func(t *testing.T) {
			inst.Domain = "alice.example.com"
			member := &Member{
				Email:    "nonexistent@other.com",
				Instance: "https://nonexistent.other.com",
			}

			// Contact doesn't exist, should not be trusted
			require.False(t, IsTrustedMember(inst, member))
		})

		t.Run("untrusted contact should not be trusted", func(t *testing.T) {
			// Create a contact but don't mark as trusted
			c := contact.New()
			c.M["email"] = []interface{}{
				map[string]interface{}{"address": "dave@external.com"},
			}
			require.NoError(t, couchdb.CreateDoc(inst, c))
			// Note: NOT calling c.MarkAsTrusted(inst)

			inst.Domain = "alice.example.com"
			member := &Member{
				Email:    "dave@external.com",
				Instance: "https://dave.external.com",
			}

			// Contact exists but is not marked as trusted
			require.False(t, IsTrustedMember(inst, member))
		})
	})

	t.Run("organization membership", func(t *testing.T) {
		prevDefault := cfg.Contexts[config.DefaultInstanceContext]
		prevOrgID := inst.OrgID
		t.Cleanup(func() {
			cfg.Contexts[config.DefaultInstanceContext] = prevDefault
			inst.OrgID = prevOrgID
		})
		setContext := func(commonContacts, autoAccept bool) {
			cfg.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
				"common_contacts": commonContacts,
				"sharing":         map[string]interface{}{"auto_accept_trusted": autoAccept},
			}
		}

		sender := testutils.NewSetup(t, t.Name()+"_sender").
			GetTestInstance(&lifecycle.Options{OrgID: "org-one"})
		member := &Member{Instance: sender.PageURL("", nil)}
		inst.OrgID = "org-one"

		setContext(true, true)
		require.True(t, IsTrustedMember(inst, member))
		require.False(t, IsTrustedMember(inst, &Member{Instance: "https://unknown.example.net"}))
		require.False(t, IsTrustedMember(inst, &Member{}))
		t.Run("unregistered port", func(t *testing.T) {
			require.False(t, IsTrustedMember(inst, &Member{Instance: "https://" + sender.Domain + ":8080"}))
		})
		t.Run("instance with a port", func(t *testing.T) {
			withPort := testutils.NewSetup(t, t.Name()).GetTestInstance(&lifecycle.Options{
				Domain: "withport." + sender.Domain + ":8080",
				OrgID:  "org-one",
			})
			require.True(t, IsTrustedMember(inst, &Member{Instance: withPort.PageURL("", nil)}))
		})

		setContext(true, false)
		require.False(t, IsTrustedMember(inst, member), "auto accept is off")

		setContext(false, true)
		require.False(t, IsTrustedMember(inst, member), "common contacts are off")

		setContext(true, true)
		inst.OrgID = "org-two"
		require.False(t, IsTrustedMember(inst, member), "other organization")
		inst.OrgID = ""
		require.False(t, IsTrustedMember(inst, member), "no organization")
	})
}

func TestMarkSenderContactTrusted(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	cfg := config.GetConfig()
	prevContexts := cfg.Contexts
	cfg.Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"common_contacts": true,
			"sharing": map[string]interface{}{
				"auto_accept_trusted_contacts": true,
			},
		},
	}
	t.Cleanup(func() { cfg.Contexts = prevContexts })

	recipient := testutils.NewSetup(t, t.Name()+"_recipient").GetTestInstance()
	recipient.OrgID = "org-one"
	sender := testutils.NewSetup(t, t.Name()+"_sender").GetTestInstance(&lifecycle.Options{OrgID: "org-one"})
	member := &Member{Email: "member@example.net", Name: "Member", Instance: sender.PageURL("", nil)}

	markSenderContactTrusted(recipient, member)
	_, err := contact.FindByEmail(recipient, member.Email)
	require.ErrorIs(t, err, contact.ErrNotFound, "answering a member's sharing must not write a contact")
	existing := contact.New()
	existing.M["email"] = []interface{}{map[string]interface{}{"address": member.Email}}
	require.NoError(t, couchdb.CreateDoc(recipient, existing))
	revision := existing.Rev()
	markSenderContactTrusted(recipient, member)
	c, err := contact.FindByEmail(recipient, member.Email)
	require.NoError(t, err)
	require.Equal(t, revision, c.Rev())
	require.False(t, c.IsTrusted())

	external := &Member{Email: "external@example.net", Instance: "https://external.example.net"}
	markSenderContactTrusted(recipient, external)
	c, err = contact.FindByEmail(recipient, external.Email)
	require.NoError(t, err)
	require.True(t, c.IsTrusted())

	cfg.Contexts[config.DefaultInstanceContext].(map[string]interface{})["common_contacts"] = false
	markSenderContactTrusted(recipient, member)
	c, err = contact.FindByEmail(recipient, member.Email)
	require.NoError(t, err)
	require.True(t, c.IsTrusted())
}
