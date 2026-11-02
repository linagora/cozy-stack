package sharing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func newOrgGroupInstances(t *testing.T, centralized, common bool) (*instance.Instance, *instance.Instance, *instance.Instance) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{"propagation": map[string]interface{}{
		"centralized_org_directory": centralized, "common_contacts": common,
	}}
	t.Cleanup(func() { conf.Contexts = previous })
	orgID := fmt.Sprintf("propagation%d", time.Now().UnixNano())
	create := func(name string) *instance.Instance {
		return testutils.NewSetup(t, name+orgID).GetTestInstance(&lifecycle.Options{
			Domain: name + ".groups.localhost", OrgID: orgID, OrgDomain: orgID + ".example",
			Email: name + "@cozy.tools", ContextName: "propagation",
		})
	}
	return create(orgID), create("alice" + orgID), create("bob" + orgID)
}

func TestReconcileOrgGroupsRevokesMemberAccess(t *testing.T) {
	for _, tc := range []struct {
		name                                                                string
		centralized, common, direct, otherGroup, deleteGroup, deleteContact bool
	}{
		{name: "centralized", centralized: true},
		{name: "centralized with service", centralized: true, common: true},
		{name: "replicated"},
		{name: "replicated with service", common: true},
		{name: "direct grant survives", centralized: true, direct: true},
		{name: "other group survives", centralized: true, otherGroup: true},
		{name: "deleted group", centralized: true, deleteGroup: true},
		{name: "deleted contact", centralized: true, deleteContact: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org, owner, recipient := newOrgGroupInstances(t, tc.centralized, tc.common)
			member := orgdirectory.GroupMember{Email: recipient.Email, WorkplaceFQDN: recipient.Domain}
			require.NoError(t, orgdirectory.SyncGroupCreated(t.Context(), orgdirectory.GroupCreatedMessage{
				OrganizationID: org.OrgID, ID: "team", Name: "Team", Members: []orgdirectory.GroupMember{member},
			}))
			groupID := orgdirectory.GroupDocID(org.OrgID, "team")
			s := createDriveSharingForGroupTest(t, owner, "Group access")
			s.Members[0].Instance = owner.PageURL("", nil)
			s.Members = append(s.Members, Member{Status: MemberStatusReady, Instance: recipient.PageURL("", nil)})
			s.Credentials = []Credentials{{State: "state-1", XorKey: MakeXorKey()}}
			s.Groups = []Group{{ID: groupID, Name: "Team"}}
			s.Members[1].OnlyInGroups = !tc.direct
			s.Members[1].Groups = []int{0}
			if tc.otherGroup {
				s.Groups = append(s.Groups, Group{ID: "personal-group"})
				s.Members[1].Groups = append(s.Members[1].Groups, 1)
			}
			// Keep the sharing active so both the member and the permission can be checked.
			s.Members = append(s.Members, Member{Email: "carol@cozy.tools", Status: MemberStatusPendingInvitation})
			s.Credentials = append(s.Credentials, Credentials{})
			require.NoError(t, couchdb.UpdateDoc(owner, s))
			token, err := s.GetInteractCode(owner, &s.Members[1], 1)
			require.NoError(t, err)
			_, err = s.FindMemberByInteractCode(owner, token)
			require.NoError(t, err)
			if tc.centralized {
				_, err := contact.FindByEmail(owner, recipient.Email)
				require.ErrorIs(t, err, contact.ErrNotFound, "no local contact copy")
			}
			msg := job.OrgGroupsMessage{OrganizationID: org.OrgID, GroupID: groupID, SharingID: s.ID()}
			switch {
			case tc.deleteGroup:
				require.NoError(t, orgdirectory.SyncGroupDeleted(t.Context(), orgdirectory.GroupDeletedMessage{OrganizationID: org.OrgID, ID: "team"}))
			case tc.deleteContact:
				c, err := contact.FindByEmail(org, recipient.Email)
				require.NoError(t, err)
				require.NoError(t, couchdb.DeleteDoc(org, c))
				msg.GroupID = ""
			default:
				require.NoError(t, orgdirectory.SyncGroupMembersRemoved(t.Context(), orgdirectory.GroupMembersMessage{
					OrganizationID: org.OrgID, ID: "team", Members: []orgdirectory.GroupMember{member},
				}))
			}
			for range 2 {
				require.NoError(t, ReconcileOrgGroups(t.Context(), owner, msg, false))
			}
			if !tc.centralized {
				local, err := contact.FindByEmail(owner, recipient.Email)
				require.NoError(t, err)
				require.NoError(t, UpdateGroups(owner, job.ShareGroupMessage{ContactID: local.ID(), GroupsRemoved: []string{groupID}}))
			}
			s, err = FindSharing(owner, s.ID())
			require.NoError(t, err)
			require.NotContains(t, s.Members[1].Groups, 0)
			_, err = s.FindMemberByInteractCode(owner, token)
			if tc.direct || tc.otherGroup {
				require.NoError(t, err)
				require.Equal(t, MemberStatusReady, s.Members[1].Status)
			} else {
				require.Error(t, err, "the previous drive token no longer grants access")
				require.Equal(t, MemberStatusRevoked, s.Members[1].Status)
			}
			if tc.deleteGroup {
				require.True(t, s.Groups[0].Revoked)
			}
		})
	}
}

type orgGroupsBroker struct {
	job.Broker
	push func(prefixer.Prefixer, *job.JobRequest) (*job.Job, error)
}

func (b orgGroupsBroker) PushJob(db prefixer.Prefixer, req *job.JobRequest) (*job.Job, error) {
	return b.push(db, req)
}

func TestQueueOrgGroupSharingsPaginatesWhileSharesDisappear(t *testing.T) {
	org, owner, _ := newOrgGroupInstances(t, true, false)
	groupID := orgdirectory.GroupDocID(org.OrgID, "team")
	const count = 1005
	docs := make([]interface{}, count)
	for i := range count {
		docs[i] = &Sharing{SID: fmt.Sprintf("sharing-%04d", i), Active: true, Owner: true, Groups: []Group{{ID: groupID}}}
	}
	require.NoError(t, couchdb.BulkUpdateDocs(owner, consts.Sharings, docs, make([]interface{}, count)))
	pending := []job.OrgGroupsMessage{{OrganizationID: org.OrgID, GroupID: groupID}}
	seen := map[string]bool{}
	broker := orgGroupsBroker{push: func(db prefixer.Prefixer, req *job.JobRequest) (*job.Job, error) {
		require.Equal(t, owner.Domain, db.DomainName())
		var msg job.OrgGroupsMessage
		require.NoError(t, req.Message.Unmarshal(&msg))
		if msg.SharingID == "" {
			pending = append(pending, msg)
		} else {
			require.False(t, seen[msg.SharingID])
			seen[msg.SharingID] = true
			s, err := FindSharing(owner, msg.SharingID)
			require.NoError(t, err)
			require.NoError(t, couchdb.DeleteDoc(owner, s))
		}
		return &job.Job{}, nil
	}}
	for len(pending) > 0 {
		msg := pending[0]
		pending = pending[1:]
		require.NoError(t, QueueOrgGroupSharings(context.Background(), owner, msg, broker))
	}
	require.Equal(t, count, len(seen))
}

func TestReconcileOrgGroupsAddsMembersAndRefreshesMetadata(t *testing.T) {
	org, owner, _ := newOrgGroupInstances(t, true, false)
	require.NoError(t, orgdirectory.SyncGroupCreated(t.Context(), orgdirectory.GroupCreatedMessage{
		OrganizationID: org.OrgID, ID: "team", Name: "Team",
	}))
	groupID := orgdirectory.GroupDocID(org.OrgID, "team")
	s := createDriveSharingForGroupTest(t, owner, "Team drive")
	require.NoError(t, s.AddGroup(owner, groupID, false))
	require.NoError(t, couchdb.UpdateDoc(owner, s))
	members := orgdirectory.GroupMembersMessage{OrganizationID: org.OrgID, ID: "team", Members: []orgdirectory.GroupMember{{Email: "dave@cozy.tools", FirstName: "Dave"}}}
	require.NoError(t, orgdirectory.SyncGroupMembersAdded(t.Context(), members))
	name, color := "Engineering", "#abcdef"
	require.NoError(t, orgdirectory.SyncGroupUpdated(t.Context(), orgdirectory.GroupUpdatedMessage{
		OrganizationID: org.OrgID, ID: "team", Name: &name, Color: &color,
	}))
	msg := job.OrgGroupsMessage{OrganizationID: org.OrgID, GroupID: groupID, SharingID: s.ID()}
	for range 2 {
		require.NoError(t, ReconcileOrgGroups(t.Context(), owner, msg, false))
	}
	s, err := FindSharing(owner, s.ID())
	require.NoError(t, err)
	require.Len(t, s.Members, 2)
	require.Equal(t, MemberStatusPendingInvitation, s.Members[1].Status)
	require.Equal(t, name, s.Groups[0].Name)
	require.Equal(t, color, s.Groups[0].Color)

	// Replaying an older job reads the removal that happened since it was queued.
	require.NoError(t, orgdirectory.SyncGroupMembersRemoved(t.Context(), members))
	// Preserve the empty drive while testing later jobs.
	s.OrgDrive = true
	require.NoError(t, couchdb.UpdateDoc(owner, s))
	require.NoError(t, ReconcileOrgGroups(t.Context(), owner, msg, false))
	s, err = FindSharing(owner, s.ID())
	require.NoError(t, err)
	require.Equal(t, MemberStatusRevoked, s.Members[1].Status)
	msg.OrganizationID = "other-org"
	require.Error(t, ReconcileOrgGroups(t.Context(), owner, msg, false))
}

func TestGroupChangesResumeAfterPartialCompletion(t *testing.T) {
	_, inst, _ := newOrgGroupInstances(t, true, false)
	group := createGroup(t, inst, "Friends")
	c := createContactInGroups(t, inst, "Bob", []string{group.ID()})
	s := createDriveSharingForGroupTest(t, inst, "Pending invitation")
	s.OrgDrive = true
	require.NoError(t, s.AddGroup(inst, group.ID(), false))
	require.NoError(t, couchdb.UpdateDoc(inst, s))
	require.Equal(t, MemberStatusMailNotSent, s.Members[1].Status)
	// The membership was saved, but sending its invitation has not completed.
	require.NoError(t, s.AddMemberToGroup(inst, 0, c))
	require.Equal(t, MemberStatusPendingInvitation, s.Members[1].Status)

	s.Members[1].Status = MemberStatusReady
	s.Members[1].Instance = ""
	require.NoError(t, couchdb.UpdateDoc(inst, s))
	require.Error(t, s.RevokeGroup(inst, 0), "an invalid ready recipient cannot have its OAuth client revoked")
	s, err := FindSharing(inst, s.ID())
	require.NoError(t, err)
	require.False(t, s.Groups[0].Revoked)
	require.Equal(t, []int{0}, s.Members[1].Groups)
	// A retry must also finish cleanup when credentials were already revoked.
	s.Members[1].Status = MemberStatusRevoked
	s.Credentials[0] = Credentials{}
	require.NoError(t, couchdb.UpdateDoc(inst, s))
	require.NoError(t, s.RevokeGroup(inst, 0))
	require.True(t, s.Groups[0].Revoked)
	require.Empty(t, s.Members[1].Groups)
}

func TestReconcileOrgGroupAddedByRecipient(t *testing.T) {
	org, recipient, _ := newOrgGroupInstances(t, true, false)
	previousMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = previousMode })
	require.NoError(t, orgdirectory.SyncGroupCreated(t.Context(), orgdirectory.GroupCreatedMessage{OrganizationID: org.OrgID, ID: "team", Name: "Team"}))
	groupID := orgdirectory.GroupDocID(org.OrgID, "team")
	var revocations atomic.Int32
	ownerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		revocations.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ownerServer.Close)
	s := &Sharing{
		Active: true, Drive: true,
		Members: []Member{
			{Status: MemberStatusOwner, Instance: ownerServer.URL},
			{Status: MemberStatusReady, Instance: recipient.PageURL("", nil)},
		},
		Groups:      []Group{{ID: groupID, AddedBy: 1, Name: "Old name"}},
		Credentials: []Credentials{{AccessToken: &auth.AccessToken{AccessToken: "test-token"}}},
	}
	require.NoError(t, couchdb.CreateDoc(recipient, s))
	msg := job.OrgGroupsMessage{OrganizationID: org.OrgID, GroupID: groupID, SharingID: s.ID()}
	require.NoError(t, ReconcileOrgGroups(t.Context(), recipient, msg, false))
	s, err := FindSharing(recipient, s.ID())
	require.NoError(t, err)
	require.Equal(t, "Team", s.Groups[0].Name)
	require.NoError(t, orgdirectory.SyncGroupDeleted(t.Context(), orgdirectory.GroupDeletedMessage{OrganizationID: org.OrgID, ID: "team"}))
	for range 2 {
		require.NoError(t, ReconcileOrgGroups(t.Context(), recipient, msg, false))
	}
	require.EqualValues(t, 1, revocations.Load(), "delegate to the external owner only once")
	s, err = FindSharing(recipient, s.ID())
	require.NoError(t, err)
	require.True(t, s.Groups[0].Revoked)
}

func TestReconcileOrgGroupsRetriesRecipientNotification(t *testing.T) {
	org, owner, _ := newOrgGroupInstances(t, true, false)
	previousMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = previousMode })
	require.NoError(t, orgdirectory.SyncGroupCreated(t.Context(), orgdirectory.GroupCreatedMessage{OrganizationID: org.OrgID, ID: "team", Name: "New name"}))
	groupID := orgdirectory.GroupDocID(org.OrgID, "team")
	var notifications atomic.Int32
	recipient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if notifications.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(recipient.Close)
	s := &Sharing{
		Active: true, Owner: true,
		Groups:      []Group{{ID: groupID, Name: "Old name"}},
		Members:     []Member{{Status: MemberStatusOwner}, {Status: MemberStatusReady, Instance: recipient.URL}},
		Credentials: []Credentials{{AccessToken: &auth.AccessToken{AccessToken: "test-token"}}},
	}
	require.NoError(t, couchdb.CreateDoc(owner, s))
	msg := job.OrgGroupsMessage{OrganizationID: org.OrgID, GroupID: groupID, SharingID: s.ID()}
	require.Error(t, ReconcileOrgGroups(t.Context(), owner, msg, false))
	s, err := FindSharing(owner, s.ID())
	require.NoError(t, err)
	require.Equal(t, "New name", s.Groups[0].Name)
	require.NoError(t, ReconcileOrgGroups(t.Context(), owner, msg, true), "retry notification even after saving metadata")
	require.EqualValues(t, 2, notifications.Load())
	require.NoError(t, ReconcileOrgGroups(t.Context(), owner, msg, false))
	require.EqualValues(t, 2, notifications.Load(), "no notification when nothing changed")
}

func TestReconcileOrgGroupsRevokesDespiteFailedAddition(t *testing.T) {
	org, owner, _ := newOrgGroupInstances(t, true, false)
	require.NoError(t, orgdirectory.SyncGroupCreated(t.Context(), orgdirectory.GroupCreatedMessage{
		OrganizationID: org.OrgID, ID: "team", Name: "Team",
		Members: []orgdirectory.GroupMember{{Email: "dave@cozy.tools", FirstName: "Dave"}},
	}))
	groupID := orgdirectory.GroupDocID(org.OrgID, "team")
	s := createDriveSharingForGroupTest(t, owner, "Changing membership")
	s.OrgDrive = true
	s.Groups = []Group{{ID: groupID}, {ID: "other-group", ReadOnly: true}}
	s.Members = append(s.Members,
		Member{Email: "bob@cozy.tools", Status: MemberStatusPendingInvitation, OnlyInGroups: true, Groups: []int{0}},
		Member{Email: "dave@cozy.tools", Status: MemberStatusPendingInvitation, OnlyInGroups: true, Groups: []int{1}, ReadOnly: true},
	)
	s.Credentials = []Credentials{{}, {}}
	require.NoError(t, couchdb.UpdateDoc(owner, s))
	err := ReconcileOrgGroups(t.Context(), owner, job.OrgGroupsMessage{OrganizationID: org.OrgID, GroupID: groupID, SharingID: s.ID()}, false)
	require.NoError(t, err, "a role conflict is logged, as retrying cannot fix it")
	s, err = FindSharing(owner, s.ID())
	require.NoError(t, err)
	require.Equal(t, MemberStatusRevoked, s.Members[1].Status, "an incompatible addition must not delay removing access")
}

func TestReconcileOrgGroupsDoesNotTreatMissingDatabaseAsGroupDeletion(t *testing.T) {
	org, owner, _ := newOrgGroupInstances(t, true, false)
	require.NoError(t, orgdirectory.SyncGroupCreated(t.Context(), orgdirectory.GroupCreatedMessage{
		OrganizationID: org.OrgID, ID: "team", Name: "Team",
		Members: []orgdirectory.GroupMember{{Email: "bob@cozy.tools", FirstName: "Bob"}},
	}))
	groupID := orgdirectory.GroupDocID(org.OrgID, "team")
	s := createDriveSharingForGroupTest(t, owner, "Directory unavailable")
	require.NoError(t, s.AddGroup(owner, groupID, false))
	require.NoError(t, couchdb.UpdateDoc(owner, s))
	require.NoError(t, couchdb.DeleteDB(org, consts.Groups))
	err := ReconcileOrgGroups(t.Context(), owner, job.OrgGroupsMessage{OrganizationID: org.OrgID, GroupID: groupID, SharingID: s.ID()}, false)
	require.True(t, couchdb.IsNoDatabaseError(err), "%v", err)
	s, err = FindSharing(owner, s.ID())
	require.NoError(t, err)
	require.False(t, s.Groups[0].Revoked)
	require.Equal(t, []int{0}, s.Members[1].Groups)
}

func TestReconcileOrgGroupsMatchesEmailsCaseInsensitively(t *testing.T) {
	org, owner, _ := newOrgGroupInstances(t, true, false)
	require.NoError(t, orgdirectory.SyncGroupCreated(t.Context(), orgdirectory.GroupCreatedMessage{
		OrganizationID: org.OrgID, ID: "team", Name: "Team",
		Members: []orgdirectory.GroupMember{{Email: "Dave@cozy.tools", FirstName: "Dave"}},
	}))
	groupID := orgdirectory.GroupDocID(org.OrgID, "team")
	s := createDriveSharingForGroupTest(t, owner, "Email case")
	s.OrgDrive = true
	s.Groups = []Group{{ID: groupID, Name: "Team"}}
	s.Members = append(s.Members, Member{Email: "dave@cozy.tools", Status: MemberStatusPendingInvitation, OnlyInGroups: true, Groups: []int{0}})
	s.Credentials = []Credentials{{}}
	require.NoError(t, couchdb.UpdateDoc(owner, s))
	require.NoError(t, ReconcileOrgGroups(t.Context(), owner, job.OrgGroupsMessage{OrganizationID: org.OrgID, GroupID: groupID, SharingID: s.ID()}, false))
	s, err := FindSharing(owner, s.ID())
	require.NoError(t, err)
	require.Len(t, s.Members, 2, "the same person is not invited twice")
	require.Equal(t, MemberStatusPendingInvitation, s.Members[1].Status)
}

func TestAddMemberToGroupResendsOnlyItsOwnInvitation(t *testing.T) {
	_, inst, _ := newOrgGroupInstances(t, true, false)
	group := createGroup(t, inst, "Friends")
	bob := createContactInGroups(t, inst, "Bob", []string{group.ID()})
	s := createDriveSharingForGroupTest(t, inst, "Another pending invitation")
	s.OrgDrive = true
	s.Groups = []Group{{ID: group.ID(), Name: "Friends"}}
	s.Members = append(s.Members,
		Member{Email: "bob@cozy.tools", Status: MemberStatusPendingInvitation, OnlyInGroups: true, Groups: []int{0}},
		Member{Email: "carol@cozy.tools", Status: MemberStatusMailNotSent, OnlyInGroups: true, Groups: []int{0}},
	)
	s.Credentials = []Credentials{{}, {}}
	require.NoError(t, couchdb.UpdateDoc(inst, s))
	require.NoError(t, s.AddMemberToGroup(inst, 0, bob))
	s, err := FindSharing(inst, s.ID())
	require.NoError(t, err)
	require.Equal(t, MemberStatusMailNotSent, s.Members[2].Status, "Bob's change does not resend Carol's invitation")
}
