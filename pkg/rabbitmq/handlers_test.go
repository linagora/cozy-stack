package rabbitmq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/banner"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/model/settings/common"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserCreatedHandlerStoresMatrixID checks that the Matrix ID a user.created
// message carries lands in the instance settings document, which is where
// buildRequest reads it from.
func TestUserCreatedHandlerStoresMatrixID(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	newInstance := func(t *testing.T) string {
		t.Helper()
		domain := fmt.Sprintf("matrix-id-%d.example", time.Now().UnixNano())
		inst, err := lifecycle.Create(&lifecycle.Options{
			Domain: domain,
			Email:  "alice@example.org",
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(domain) })
		return inst.Domain
	}

	storedMatrixID := func(t *testing.T, domain string) string {
		t.Helper()
		inst, err := lifecycle.GetInstance(domain)
		require.NoError(t, err)
		settings, err := inst.SettingsDocument()
		require.NoError(t, err)
		id, _ := settings.M["matrix_id"].(string)
		return id
	}

	handle := func(t *testing.T, domain, matrixID string) error {
		t.Helper()
		body, err := json.Marshal(rabbitmq.UserCreatedMessage{
			TwakeID:       "alice",
			WorkplaceFqdn: domain,
			MatrixID:      matrixID,
		})
		require.NoError(t, err)

		return rabbitmq.NewUserCreatedHandler().
			Handle(context.Background(), amqp.Delivery{Body: body})
	}

	t.Run("stores the matrix id it receives", func(t *testing.T) {
		domain := newInstance(t)

		require.NoError(t, handle(t, domain, "@al.ice:example.org"))
		require.Equal(t, "@al.ice:example.org", storedMatrixID(t, domain))
	})

	t.Run("a redelivery leaves the same value in place", func(t *testing.T) {
		domain := newInstance(t)

		require.NoError(t, handle(t, domain, "@al.ice:example.org"))
		require.NoError(t, handle(t, domain, "@al.ice:example.org"))
		require.Equal(t, "@al.ice:example.org", storedMatrixID(t, domain))
	})

	t.Run("a malformed matrix id is dropped, not stored", func(t *testing.T) {
		domain := newInstance(t)

		require.NoError(t, handle(t, domain, "al.ice@example.org"))
		require.Empty(t, storedMatrixID(t, domain))
	})

	t.Run("a message without a matrix id stores nothing", func(t *testing.T) {
		domain := newInstance(t)

		require.NoError(t, handle(t, domain, ""))
		require.Empty(t, storedMatrixID(t, domain))
	})
}

func TestUserCreatedHandlerStoresEmail(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	contextName := "email-test"
	conf := config.GetConfig()
	conf.Authentication = map[string]interface{}{
		contextName: map[string]interface{}{"disable_password_authentication": true},
	}

	domain := fmt.Sprintf("email-%d.example", time.Now().UnixNano())
	_, err := lifecycle.Create(&lifecycle.Options{
		Domain:      domain,
		Email:       "alice@example.org",
		ContextName: contextName,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(domain) })

	email := fmt.Sprintf("Alice.%d@Acme.example", time.Now().UnixNano())
	body, err := json.Marshal(rabbitmq.UserCreatedMessage{
		TwakeID:       "alice",
		WorkplaceFqdn: domain,
		InternalEmail: " " + email + " ",
	})
	require.NoError(t, err)
	require.NoError(t, rabbitmq.NewUserCreatedHandler().
		Handle(context.Background(), amqp.Delivery{Body: body}))

	found, err := lifecycle.GetInstanceByEmail(strings.ToUpper(email))
	require.NoError(t, err)
	assert.Equal(t, domain, found.Domain)
	assert.Equal(t, strings.ToLower(email), found.Email)
	settingsEmail, err := found.SettingsEMail()
	require.NoError(t, err)
	assert.Equal(t, email, settingsEmail)

	_, err = lifecycle.GetInstanceByEmail("nobody@acme.example")
	assert.ErrorIs(t, err, instance.ErrNotFound)

	// A taken email is acked and left out, not redelivered forever.
	other := "other-" + domain
	_, err = lifecycle.Create(&lifecycle.Options{Domain: other, ContextName: contextName})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(other) })
	body, err = json.Marshal(rabbitmq.UserCreatedMessage{
		TwakeID:       "alice2",
		WorkplaceFqdn: other,
		InternalEmail: email,
	})
	require.NoError(t, err)
	require.NoError(t, rabbitmq.NewUserCreatedHandler().
		Handle(context.Background(), amqp.Delivery{Body: body}))
	got, err := lifecycle.GetInstance(other)
	require.NoError(t, err)
	assert.Empty(t, got.Email)
}

func TestUserCreatedHandlerPatchesEmailAndMatrixIDTogether(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	domain := fmt.Sprintf("email-matrix-%d.example", time.Now().UnixNano())
	_, err := lifecycle.Create(&lifecycle.Options{Domain: domain, Email: "before@" + domain})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(domain) })

	cfg := config.GetConfig()
	previousSettings := cfg.CommonSettings
	previousHTTP := common.DoCommonHTTP
	previousRemote := common.GetRemoteCommonSettings
	t.Cleanup(func() {
		cfg.CommonSettings = previousSettings
		common.DoCommonHTTP = previousHTTP
		common.GetRemoteCommonSettings = previousRemote
	})
	cfg.CommonSettings = map[string]config.CommonSettings{
		config.DefaultInstanceContext: {URL: "http://example.org", Token: "token"},
	}
	var requests []common.UserSettingsRequest
	common.DoCommonHTTP = func(method, urlStr, token string, body []byte) error {
		var request common.UserSettingsRequest
		if err := json.Unmarshal(body, &request); err != nil {
			return err
		}
		requests = append(requests, request)
		return nil
	}
	common.GetRemoteCommonSettings = func(inst *instance.Instance) (*common.UserSettingsRequest, error) {
		return &common.UserSettingsRequest{Version: inst.CommonSettingsVersion}, nil
	}

	email := "Alice@" + domain
	for _, matrixID := range []string{" @al.ice:example.org ", "@al.ice:example.org", "", "invalid"} {
		requests = nil
		body, err := json.Marshal(rabbitmq.UserCreatedMessage{
			TwakeID:       "alice",
			WorkplaceFqdn: domain,
			InternalEmail: " " + email + " ",
			MatrixID:      matrixID,
		})
		require.NoError(t, err)
		require.NoError(t, rabbitmq.NewUserCreatedHandler().Handle(context.Background(), amqp.Delivery{Body: body}))

		require.Len(t, requests, 1, "one update for matrix id %q", matrixID)
		require.Equal(t, email, requests[0].Payload.Email)
		require.Equal(t, "@al.ice:example.org", requests[0].Payload.MatrixID)
		inst, err := lifecycle.GetInstance(domain)
		require.NoError(t, err)
		require.Equal(t, strings.ToLower(email), inst.Email)
		settings, err := inst.SettingsDocument()
		require.NoError(t, err)
		require.Equal(t, email, settings.M["email"])
		require.Equal(t, "@al.ice:example.org", settings.M["matrix_id"])
	}
}

func TestOrganizationDirectoryFlags(t *testing.T) {
	for _, tc := range []struct {
		name                string
		centralized, common bool
	}{
		{name: "replicated without service"},
		{name: "replicated with service", common: true},
		{name: "centralized without service", centralized: true},
		{name: "centralized with service", centralized: true, common: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.UseTestFile(t)
			testutils.NeedCouchdb(t)
			conf := config.GetConfig()
			previous := conf.Contexts
			conf.Contexts = map[string]interface{}{"directory-flags": map[string]interface{}{
				"common_contacts": tc.common, "centralized_org_directory": tc.centralized,
			}}
			t.Cleanup(func() { conf.Contexts = previous })
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			orgID := "flags" + suffix
			newInstance := func(domain, email, name string) *instance.Instance {
				inst, err := lifecycle.Create(&lifecycle.Options{
					Domain: domain, OrgDomain: orgID + ".example", OrgID: orgID,
					Email: email, PublicName: name, ContextName: "directory-flags",
				})
				require.NoError(t, err)
				t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
				return inst
			}
			org := newInstance(orgID+".local", "org-"+suffix+"@acme.example", "Organization")
			bobEmail := "bob-" + suffix + "@acme.example"
			bob := newInstance("bob-"+suffix+".local", bobEmail, "Bob")
			require.NoError(t, orgdirectory.SyncGroupCreated(testCtx(t), orgdirectory.GroupCreatedMessage{
				OrganizationID: orgID, ID: "team", Name: "Team",
				Members: []orgdirectory.GroupMember{{Email: bobEmail, FirstName: "Bob", WorkplaceFQDN: bob.Domain}},
			}))
			aliceEmail := "alice-" + suffix + "@acme.example"
			alice := newInstance("alice-"+suffix+".local", "before-"+aliceEmail, "Alice")
			body, err := json.Marshal(rabbitmq.UserCreatedMessage{
				TwakeID: "alice", WorkplaceFqdn: alice.Domain, InternalEmail: aliceEmail,
				MatrixID: "@alice:acme.example", OrganizationID: orgID,
			})
			require.NoError(t, err)
			require.NoError(t, rabbitmq.NewUserCreatedHandler().Handle(context.Background(), amqp.Delivery{Body: body}))
			settings, err := alice.SettingsDocument()
			require.NoError(t, err)
			assert.Equal(t, aliceEmail, settings.M["email"])
			assert.Equal(t, "@alice:acme.example", settings.M["matrix_id"])
			orgContact, err := contact.FindByEmail(org, aliceEmail)
			require.NoError(t, err, "B2B events populate the org without the contacts service")
			assert.Equal(t, alice.PageURL("", nil), orgContact.PrimaryCozyURL())
			_, err = contact.FindByEmail(bob, aliceEmail)
			if tc.centralized {
				require.ErrorIs(t, err, contact.ErrNotFound)
			} else {
				require.NoError(t, err)
			}
			_, err = contact.FindByEmail(alice, bobEmail)
			if tc.centralized {
				require.ErrorIs(t, err, contact.ErrNotFound)
			} else {
				require.NoError(t, err)
			}
			_, err = contact.FindGroup(alice, orgdirectory.GroupDocID(orgID, "team"))
			if tc.centralized {
				require.True(t, couchdb.IsNotFoundError(err))
			} else {
				require.NoError(t, err)
			}

			require.NoError(t, rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
				WorkplaceFqdn: alice.Domain, InternalEmail: aliceEmail, OrganizationID: orgID,
			}))
			_, err = contact.FindByEmail(org, aliceEmail)
			require.ErrorIs(t, err, contact.ErrNotFound)
			if tc.centralized {
				require.NoError(t, lifecycle.Destroy(org.Domain))
				err = rabbitmq.NewUserCreatedHandler().Handle(context.Background(), amqp.Delivery{Body: body})
				require.ErrorIs(t, err, instance.ErrNotFound, "do not acknowledge events without an org destination")
			}
		})
	}
}

func TestDeletedFeedContactWithoutService(t *testing.T) {
	for _, common := range []bool{false, true} {
		t.Run(fmt.Sprintf("service=%t", common), func(t *testing.T) {
			config.UseTestFile(t)
			testutils.NeedCouchdb(t)
			conf := config.GetConfig()
			previous := conf.Contexts
			conf.Contexts = map[string]interface{}{config.DefaultInstanceContext: map[string]interface{}{"common_contacts": common}}
			t.Cleanup(func() { conf.Contexts = previous })
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			orgID := "delete-flags" + suffix
			inst := createInstanceInOrg(t, orgID+".local", orgID+".example", orgID, "", "Organization")
			c := createContact(t, inst, "alice@acme.example", "https://alice.example", true, "Alice")
			c.M[contact.CardDAVPathKey] = "addressbooks/org/alice.vcf"
			require.NoError(t, couchdb.UpdateDoc(inst, c))
			require.NoError(t, rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
				WorkplaceFqdn: "alice.example", InternalEmail: "alice@acme.example", OrganizationID: orgID,
			}))
			_, err := contact.Find(inst, c.ID())
			if common {
				require.NoError(t, err)
			} else {
				require.True(t, couchdb.IsNotFoundError(err))
			}
		})
	}
}

func TestBannerCommandHandler(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	contextName := "banner-command-handler-test"
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{
		contextName: map[string]interface{}{
			"banner": map[string]interface{}{
				"enabled":            true,
				"command_categories": []interface{}{"billing"},
			},
		},
	}
	t.Cleanup(func() { conf.Contexts = previous })

	domain := fmt.Sprintf("banner-handler-%d.example", time.Now().UnixNano())
	_, err := lifecycle.Create(&lifecycle.Options{
		Domain:      domain,
		Email:       "alice@example.org",
		ContextName: contextName,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(domain) })

	fixture := func(t *testing.T, name string, revision int64) []byte {
		t.Helper()
		raw, err := os.ReadFile("../../model/banner/testdata/" + name + ".json")
		require.NoError(t, err)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &payload))
		payload["workplaceFqdn"] = domain
		payload["revision"] = revision
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		return body
	}

	handle := func(t *testing.T, key string, body []byte) error {
		t.Helper()
		return rabbitmq.NewBannerCommandHandler().
			Handle(context.Background(), amqp.Delivery{RoutingKey: key, Body: body})
	}

	stored := func(t *testing.T) *banner.Banner {
		t.Helper()
		inst, err := lifecycle.GetInstance(domain)
		require.NoError(t, err)
		doc, err := banner.Stored(inst, banner.CategoryBilling)
		require.NoError(t, err)
		return doc
	}

	t.Run("the routing key materializes, then clears", func(t *testing.T) {
		require.NoError(t, handle(t, rabbitmq.RoutingKeyBannerMaterialize, fixture(t, "materialize", 1)))
		require.NotNil(t, stored(t))

		require.NoError(t, handle(t, rabbitmq.RoutingKeyBannerClear, fixture(t, "clear", 2)))
		require.NotNil(t, stored(t))
		require.True(t, stored(t).Cleared)
		require.NotNil(t, stored(t).EndsAt)
		assert.True(t, stored(t).EndsAt.Before(time.Now()))
	})

	t.Run("a payload that does not parse fails", func(t *testing.T) {
		err := handle(t, rabbitmq.RoutingKeyBannerMaterialize, []byte("{"))
		require.Error(t, err)
	})

	t.Run("an unexpected routing key fails", func(t *testing.T) {
		err := handle(t, "banner.something", fixture(t, "materialize", 3))
		require.Error(t, err)
	})

	t.Run("an invalid command fails", func(t *testing.T) {
		body := fixture(t, "materialize", 4)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &payload))
		payload["severity"] = "critical"
		body, err := json.Marshal(payload)
		require.NoError(t, err)

		err = handle(t, rabbitmq.RoutingKeyBannerMaterialize, body)
		require.Error(t, err)
		assert.ErrorIs(t, err, banner.ErrInvalidCommand)
	})

	t.Run("an oversized body is rejected before decoding", func(t *testing.T) {
		err := handle(t, rabbitmq.RoutingKeyBannerMaterialize, []byte(strings.Repeat(" ", banner.MaxCommandBytes+1)))
		assert.ErrorIs(t, err, banner.ErrInvalidCommand)
	})

	t.Run("invalid times never advance desired state", func(t *testing.T) {
		body := fixture(t, "materialize", 100)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &payload))
		payload["timestamp"] = 1788944400000
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		err = handle(t, rabbitmq.RoutingKeyBannerMaterialize, body)
		assert.ErrorIs(t, err, banner.ErrInvalidCommand)
		// Reusing that revision with valid content succeeds only if the bad
		// command was rejected before the desired-state write.
		require.NoError(t, handle(t, rabbitmq.RoutingKeyBannerMaterialize, fixture(t, "materialize", 100)))
	})

	t.Run("a target that is not here yet is redelivered", func(t *testing.T) {
		body := fixture(t, "materialize", 5)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &payload))
		payload["workplaceFqdn"] = fmt.Sprintf("provisioning-%d.example", time.Now().UnixNano())
		body, err := json.Marshal(payload)
		require.NoError(t, err)

		err = handle(t, rabbitmq.RoutingKeyBannerMaterialize, body)
		require.Error(t, err, "a workplace still being provisioned deserves another attempt")
	})
}
