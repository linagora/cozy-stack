package share

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	_ "github.com/cozy/cozy-stack/worker/mails"
)

func TestOrganizationGroupEventsReachMemberSharings(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	conf := config.GetConfig()
	previous := conf.Contexts
	propagate := map[string]interface{}{
		"centralized_org_directory": true, "common_contacts": true, "org_groups_propagation_delay": "50ms",
	}
	conf.Contexts = map[string]interface{}{"propagate": propagate}
	t.Cleanup(func() { conf.Contexts = previous })
	orgID := fmt.Sprintf("events%d", time.Now().UnixNano())
	create := func(name string) *instance.Instance {
		return testutils.NewSetup(t, name).GetTestInstance(&lifecycle.Options{
			Domain: name + ".events.localhost", OrgID: orgID, OrgDomain: orgID + ".example", ContextName: "propagate",
		})
	}
	org, owner := create(orgID), create("alice"+orgID)
	member := orgdirectory.GroupMember{Email: "bob@cozy.tools", FirstName: "Bob"}
	require.NoError(t, orgdirectory.SyncGroupCreated(t.Context(), orgdirectory.GroupCreatedMessage{
		OrganizationID: orgID, ID: "team", Name: "Team", Members: []orgdirectory.GroupMember{member},
	}))
	groupID := orgdirectory.GroupDocID(orgID, "team")
	s := &sharing.Sharing{
		Active: true, Owner: true, Drive: true, OrgDrive: true,
		Groups: []sharing.Group{{ID: groupID, Name: "Team"}},
		Members: []sharing.Member{
			{Status: sharing.MemberStatusOwner},
			{Status: sharing.MemberStatusPendingInvitation, Email: member.Email, Name: "Bob", OnlyInGroups: true, Groups: []int{0}},
		},
		Credentials: []sharing.Credentials{{}},
		Rules:       []sharing.Rule{{DocType: consts.Files, Values: []string{"test-directory"}}},
	}
	require.NoError(t, couchdb.CreateDoc(owner, s))
	handler := rabbitmq.NewB2BGroupLifecycleHandler()
	deliver := func(key string, message interface{}) {
		t.Helper()
		body, err := json.Marshal(message)
		require.NoError(t, err)
		for range 2 {
			require.NoError(t, handler.Handle(t.Context(), amqp.Delivery{RoutingKey: key, Body: body}))
		}
	}
	await := func(check func(*sharing.Sharing) bool) {
		t.Helper()
		require.Eventually(t, func() bool {
			stored, err := sharing.FindSharing(owner, s.ID())
			return err == nil && check(stored)
		}, 15*time.Second, 20*time.Millisecond)
	}
	membership := orgdirectory.GroupMembersMessage{OrganizationID: orgID, ID: "team", Members: []orgdirectory.GroupMember{member}}
	removed := func(s *sharing.Sharing) bool { return s.Members[1].Status == sharing.MemberStatusRevoked }
	add := func() {
		deliver(rabbitmq.RoutingKeyB2BGroupMemberAdded, membership)
		await(func(s *sharing.Sharing) bool { return s.Members[1].Status == sharing.MemberStatusPendingInvitation })
	}
	// A burst of changes within the window shares one propagation run.
	propagate["org_groups_propagation_delay"] = "2s"
	for range 5 {
		deliver(rabbitmq.RoutingKeyB2BGroupMemberRemoved, membership)
	}
	triggers, err := job.System().GetAllTriggers(org)
	require.NoError(t, err)
	require.Len(t, slices.DeleteFunc(triggers, func(tr job.Trigger) bool {
		return tr.Infos().WorkerType != job.OrgGroupsWorker
	}), 1)
	propagate["org_groups_propagation_delay"] = "50ms"
	await(removed)
	name, color := "Engineering", "#123456"
	deliver(rabbitmq.RoutingKeyB2BGroupUpdated, orgdirectory.GroupUpdatedMessage{OrganizationID: orgID, ID: "team", Name: &name, Color: &color})
	await(func(s *sharing.Sharing) bool { return s.Groups[0].Name == name && s.Groups[0].Color == color })

	add()
	c, err := contact.FindByEmail(org, member.Email)
	require.NoError(t, err)
	path := "addressbooks/org/bob.vcf"
	c.M[contact.CardDAVPathKey] = path
	require.NoError(t, couchdb.UpdateDoc(org, c))
	body, err := json.Marshal(map[string]interface{}{"action": "DELETE", "path": path, "audience": map[string]string{"domain": org.OrgDomain}})
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, rabbitmq.NewCommonContactsHandler().Handle(t.Context(), amqp.Delivery{Body: body}))
	}
	await(removed)

	add()
	c, err = contact.FindByEmail(org, member.Email)
	require.NoError(t, err)
	c.M[contact.CardDAVPathKey] = path
	require.NoError(t, couchdb.UpdateDoc(org, c))
	for range 2 {
		_, err = orgdirectory.ReconcileOrganizationContacts(t.Context(), orgID, time.Now().Add(-time.Second), 0, nil, false)
		require.NoError(t, err)
	}
	await(removed)

	add()
	userBody, err := json.Marshal(rabbitmq.UserDeletedMessage{OrganizationID: orgID, WorkplaceFqdn: "bob.events.localhost", InternalEmail: member.Email})
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, rabbitmq.NewUserDeletedHandler().Handle(t.Context(), amqp.Delivery{Body: userBody}))
	}
	await(removed)

	add()
	deliver(rabbitmq.RoutingKeyB2BGroupDeleted, orgdirectory.GroupDeletedMessage{OrganizationID: orgID, ID: "team"})
	await(func(s *sharing.Sharing) bool { return removed(s) && s.Groups[0].Revoked })
}
