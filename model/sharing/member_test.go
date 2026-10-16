package sharing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/messaging"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldRetryDelegatedRequest(t *testing.T) {
	tests := []struct {
		name     string
		response *http.Response
		expected bool
	}{
		{name: "missing response"},
		{name: "unauthorized", response: &http.Response{StatusCode: http.StatusUnauthorized}, expected: true},
		{name: "forbidden", response: &http.Response{StatusCode: http.StatusForbidden}, expected: true},
		{name: "moved", response: &http.Response{StatusCode: http.StatusGone}, expected: true},
		{name: "bad request", response: &http.Response{StatusCode: http.StatusBadRequest}},
		{name: "conflict", response: &http.Response{StatusCode: http.StatusConflict}},
		{name: "not found", response: &http.Response{StatusCode: http.StatusNotFound}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, shouldRetryDelegatedRequest(tt.response))
		})
	}
}

func TestPreserveDelegatedResponseError(t *testing.T) {
	t.Run("prefers the response returned after token refresh", func(t *testing.T) {
		preRefreshRes := &http.Response{StatusCode: http.StatusUnauthorized}
		preRefreshErr := &request.Error{Detail: `{"errors":[{"status":"401","title":"Unauthorized"}]}`}
		res := &http.Response{StatusCode: http.StatusConflict}
		err := &request.Error{Detail: `{"errors":[{"status":"409","title":"Conflict","detail":"already active"}]}`}

		got := preserveDelegatedResponseError(res, err, preRefreshRes, preRefreshErr)

		var apiErr *jsonapi.Error
		require.ErrorAs(t, got, &apiErr)
		require.Equal(t, http.StatusConflict, apiErr.Status)
		require.Equal(t, "already active", apiErr.Detail)
	})

	t.Run("falls back to the pre-refresh response when refresh fails", func(t *testing.T) {
		preRefreshRes := &http.Response{StatusCode: http.StatusForbidden}
		preRefreshErr := &request.Error{Detail: `{"errors":[{"status":"403","title":"Forbidden","detail":"read only"}]}`}
		refreshErr := fmt.Errorf("refresh failed")

		got := preserveDelegatedResponseError(nil, refreshErr, preRefreshRes, preRefreshErr)

		var apiErr *jsonapi.Error
		require.ErrorAs(t, got, &apiErr)
		require.Equal(t, http.StatusForbidden, apiErr.Status)
		require.Equal(t, "read only", apiErr.Detail)
	})
}

// TestSendDelegatedReaddsRevokedGroup checks that a group revoked on the
// local copy is re-added by SendDelegated, as the sharer does in AddGroup,
// so that the group indexes stay aligned between the two sides.
func TestSendDelegatedReaddsRevokedGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	// safehttp blocks loopback addresses outside dev mode; enable it so the
	// in-memory httptest server is reachable.
	oldBuildMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = oldBuildMode })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	now := time.Now()
	s := &Sharing{
		Active: true,
		Members: []Member{
			{Status: MemberStatusOwner, Instance: srv.URL},
			{Status: MemberStatusReady, Instance: "https://" + inst.Domain},
		},
		Credentials: []Credentials{{
			AccessToken: &auth.AccessToken{AccessToken: "test-token"},
		}},
		Groups: []Group{{
			ID:      "team-id",
			Name:    "Team",
			AddedBy: 1,
			Revoked: true,
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, couchdb.CreateDoc(inst, s))

	api := &APIDelegateAddContacts{groups: []Group{{ID: "team-id", Name: "Team"}}}
	require.NoError(t, s.SendDelegated(inst, api))

	require.Len(t, s.Groups, 2)
	assert.True(t, s.Groups[0].Revoked)
	assert.Equal(t, "team-id", s.Groups[1].ID)
	assert.False(t, s.Groups[1].Revoked)
	assert.Equal(t, 1, s.Groups[1].AddedBy)
}

func TestReadOnlyFlagRejectsBrokenRecipientCredentials(t *testing.T) {
	cases := []struct {
		name     string
		readOnly bool
		run      func(*Sharing) error
	}{
		{
			name: "add",
			run:  func(s *Sharing) error { return s.AddReadOnlyFlag(nil, 1) },
		},
		{
			name:     "remove",
			readOnly: true,
			run:      func(s *Sharing) error { return s.RemoveReadOnlyFlag(nil, 1) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sharing{
				SID:   "sharing-id",
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner, Instance: "https://owner.example.test"},
					{Status: MemberStatusReady, Instance: "https://recipient.example.test", ReadOnly: tc.readOnly},
				},
				Credentials: []Credentials{{}},
			}

			err := tc.run(s)

			require.ErrorIs(t, err, ErrInvalidSharing)
			require.Equal(t, tc.readOnly, s.Members[1].ReadOnly)
		})
	}
}

func TestDelegateReadOnlyFlagRejectsBrokenOwnerCredentials(t *testing.T) {
	validToken := &auth.AccessToken{AccessToken: "token"}
	cases := []struct {
		name    string
		index   int
		sharing func() *Sharing
		wantErr error
	}{
		{
			name:  "invalid index",
			index: 0,
			sharing: func() *Sharing {
				return &Sharing{
					Members: []Member{
						{Instance: "https://owner.example.test"},
						{Instance: "https://recipient.example.test"},
					},
					Credentials: []Credentials{{AccessToken: validToken}},
				}
			},
			wantErr: ErrMemberNotFound,
		},
		{
			name:  "missing credentials",
			index: 1,
			sharing: func() *Sharing {
				return &Sharing{
					Members: []Member{
						{Instance: "https://owner.example.test"},
						{Instance: "https://recipient.example.test"},
					},
				}
			},
			wantErr: ErrInvalidSharing,
		},
		{
			name:  "nil access token",
			index: 1,
			sharing: func() *Sharing {
				return &Sharing{
					Members: []Member{
						{Instance: "https://owner.example.test"},
						{Instance: "https://recipient.example.test"},
					},
					Credentials: []Credentials{{Client: &auth.Client{ClientID: "client-id"}}},
				}
			},
			wantErr: ErrInvalidSharing,
		},
		{
			name:  "missing owner instance",
			index: 1,
			sharing: func() *Sharing {
				return &Sharing{
					Members: []Member{
						{},
						{Instance: "https://recipient.example.test"},
					},
					Credentials: []Credentials{{AccessToken: validToken}},
				}
			},
			wantErr: ErrInvalidSharing,
		},
	}

	methods := map[string]func(*Sharing, int) error{
		"add":    func(s *Sharing, i int) error { return s.DelegateAddReadOnlyFlag(nil, i) },
		"remove": func(s *Sharing, i int) error { return s.DelegateRemoveReadOnlyFlag(nil, i) },
	}

	for _, tc := range cases {
		for name, run := range methods {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				require.ErrorIs(t, run(tc.sharing(), tc.index), tc.wantErr)
			})
		}
	}
}

func TestReadOnlyFlagUpdatesPendingRecipientLocally(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	require.NoError(t, couchdb.ResetDB(inst, consts.Sharings))

	cases := []struct {
		name     string
		status   string
		readOnly bool
		update   func(*Sharing) error
		want     bool
	}{
		{
			name:   "add pending invitation",
			status: MemberStatusPendingInvitation,
			update: func(s *Sharing) error {
				return s.AddReadOnlyFlag(inst, 1)
			},
			want: true,
		},
		{
			name:     "remove pending invitation",
			status:   MemberStatusPendingInvitation,
			readOnly: true,
			update: func(s *Sharing) error {
				return s.RemoveReadOnlyFlag(inst, 1)
			},
		},
		{
			name:   "add seen invitation",
			status: MemberStatusSeen,
			update: func(s *Sharing) error {
				return s.AddReadOnlyFlag(inst, 1)
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sharing{
				SID:   "sharing-" + strings.ReplaceAll(tc.name, " ", "-"),
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner, Instance: "https://owner.example.test"},
					{
						Status:   tc.status,
						Email:    "recipient@example.test",
						Instance: "https://recipient.example.test",
						ReadOnly: tc.readOnly,
					},
				},
				Credentials: []Credentials{{State: "state"}},
			}
			require.NoError(t, couchdb.CreateNamedDoc(inst, s))

			err := tc.update(s)

			require.NoError(t, err)
			require.Equal(t, tc.want, s.Members[1].ReadOnly)
			stored, err := FindSharing(inst, s.SID)
			require.NoError(t, err)
			require.Equal(t, tc.status, stored.Members[1].Status)
			require.Equal(t, tc.want, stored.Members[1].ReadOnly)
			require.Nil(t, stored.Credentials[0].AccessToken)
		})
	}
}

// TestAddReadOnlyFlagSendsReadOnlyRefreshToken checks that a recipient made
// read-only can still renew its token, with a read-only scope, to call the
// sharer later (e.g. to open an office document created by the sharer).
func TestAddReadOnlyFlagSendsReadOnlyRefreshToken(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	// safehttp blocks loopback addresses outside dev mode; enable it so the
	// in-memory httptest server is reachable.
	oldBuildMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = oldBuildMode })

	var received struct {
		Data struct {
			Attributes Credentials `json:"attributes"`
		} `json:"data"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := &Sharing{
		SID:    "sharing-readonly-refresh",
		Owner:  true,
		Active: true,
		Members: []Member{
			{Status: MemberStatusOwner, Instance: "https://" + inst.Domain},
			{Status: MemberStatusReady, Email: "recipient@example.test", Instance: srv.URL},
		},
		Credentials: []Credentials{{
			AccessToken: &auth.AccessToken{AccessToken: "token"},
		}},
	}
	require.NoError(t, couchdb.CreateNamedDoc(inst, s))

	require.NoError(t, s.AddReadOnlyFlag(inst, 1))
	require.True(t, s.Members[1].ReadOnly)

	sent := received.Data.Attributes
	require.NotNil(t, sent.Client)
	require.NotNil(t, sent.AccessToken)
	require.NotEmpty(t, sent.AccessToken.RefreshToken)

	cli, err := oauth.FindClient(inst, sent.Client.ClientID)
	require.NoError(t, err)
	claims, ok := cli.ValidToken(inst, consts.RefreshTokenAudience, sent.AccessToken.RefreshToken)
	require.True(t, ok)
	require.Equal(t, consts.Sharings+":GET:"+s.SID, claims.Scope)
}

func TestProcessAnswerRejectsBrokenPayloads(t *testing.T) {
	validToken := &auth.AccessToken{AccessToken: "token"}
	validClient := &auth.Client{ClientID: "client-id"}

	cases := []struct {
		name  string
		creds *APICredentials
	}{
		{name: "nil payload"},
		{name: "missing embedded credentials", creds: &APICredentials{}},
		{name: "missing client", creds: &APICredentials{Credentials: &Credentials{State: "state", AccessToken: validToken}}},
		{name: "missing access token", creds: &APICredentials{Credentials: &Credentials{State: "state", Client: validClient}}},
		{name: "empty access token", creds: &APICredentials{Credentials: &Credentials{State: "state", Client: validClient, AccessToken: &auth.AccessToken{}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sharing{
				SID:   "sharing-id",
				Owner: true,
				Members: []Member{
					{Status: MemberStatusOwner},
					{Status: MemberStatusPendingInvitation},
				},
				Credentials: []Credentials{{State: "state"}},
			}

			_, err := s.ProcessAnswer(nil, tc.creds)

			require.ErrorIs(t, err, ErrInvalidSharing)
			require.Equal(t, MemberStatusPendingInvitation, s.Members[1].Status)
			require.Nil(t, s.Credentials[0].Client)
			require.Nil(t, s.Credentials[0].AccessToken)
		})
	}
}

func TestDowngradeToReadOnlyRejectsBrokenPayload(t *testing.T) {
	validToken := &auth.AccessToken{AccessToken: "token"}
	validClient := &auth.Client{ClientID: "client-id"}
	s := &Sharing{
		Members: []Member{
			{Instance: "https://owner.example.test"},
			{Instance: "https://recipient.example.test"},
		},
		Credentials: []Credentials{{Client: validClient, AccessToken: validToken}},
	}

	err := s.DowngradeToReadOnly(nil, &APICredentials{Credentials: &Credentials{Client: validClient}})

	require.ErrorIs(t, err, ErrInvalidSharing)
	require.False(t, s.Members[1].ReadOnly)
	require.Same(t, validToken, s.Credentials[0].AccessToken)
}

func TestUpgradeToReadWriteRejectsBrokenPayload(t *testing.T) {
	validToken := &auth.AccessToken{AccessToken: "token"}
	validClient := &auth.Client{ClientID: "client-id"}
	s := &Sharing{
		Members: []Member{
			{Instance: "https://owner.example.test"},
			{Instance: "https://recipient.example.test", ReadOnly: true},
		},
		Credentials: []Credentials{{Client: validClient, AccessToken: validToken}},
	}

	err := s.UpgradeToReadWrite(nil, &APICredentials{Credentials: &Credentials{Client: validClient}})

	require.ErrorIs(t, err, ErrInvalidSharing)
	require.True(t, s.Members[1].ReadOnly)
	require.Same(t, validToken, s.Credentials[0].AccessToken)
}

func TestCheckSharingCredentialsReportsRecipientGaps(t *testing.T) {
	s := &Sharing{
		SID:    "sharing-id",
		Active: true,
		Owner:  false,
		Members: []Member{
			{Instance: "https://owner.example.test"},
			{Instance: "https://recipient.example.test"},
		},
		Credentials: []Credentials{{}},
	}

	checks := s.checkSharingCredentials()

	requireCheck(t, checks, "missing_oauth_client", false)
	requireCheck(t, checks, "missing_access_token", false)
}

func TestCheckSharingCredentialsReportsOwnerMismatchedCount(t *testing.T) {
	s := &Sharing{
		SID:    "sharing-id",
		Active: true,
		Owner:  true,
		Members: []Member{
			{Status: MemberStatusOwner},
			{Status: MemberStatusReady, Instance: "https://recipient.example.test"},
		},
		Credentials: nil,
	}

	checks := s.checkSharingCredentials()

	requireCheck(t, checks, "invalid_number_of_credentials", true)
}

func TestCheckSharingCredentialsSharedDriveRecipient(t *testing.T) {
	s := &Sharing{
		SID:    "sharing-id",
		Active: true,
		Owner:  false,
		Drive:  true,
		Members: []Member{
			{Instance: "https://owner.example.test"},
			{Instance: "https://recipient.example.test"},
		},
		Credentials: []Credentials{{}},
	}

	checks := s.checkSharingCredentials()
	requireCheck(t, checks, "missing_access_token", false)

	// With valid DriveToken
	s.Credentials[0].DriveToken = "drive-token"
	checks = s.checkSharingCredentials()
	require.Empty(t, checks)

	// With invalid count
	s.Credentials = nil
	checks = s.checkSharingCredentials()
	requireCheck(t, checks, "invalid_number_of_credentials", false)
}

func requireCheck(t *testing.T, checks []map[string]interface{}, typ string, owner bool) {
	t.Helper()
	for _, c := range checks {
		if c["type"] == typ && c["owner"] == owner {
			return
		}
	}
	require.Failf(t, "missing check", "expected type=%q owner=%t in %#v", typ, owner, checks)
}

func TestFindMemberByInteractCodeWithDuplicatePermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	require.NoError(t, couchdb.ResetDB(inst, consts.Permissions))

	const sharingID = "sharing-duplicate-interact-permissions"
	const aliceEmail = "alice@example.test"
	const bobEmail = "bob@example.test"

	aliceToken := "alice-interact-token"
	bobToken := "bob-interact-token"
	perms := permission.Permission{
		Permissions: permission.Set{{
			Title:  "Shared drive",
			Type:   consts.Files,
			Values: []string{"shared-drive-root"},
			Verbs:  permission.ALL,
		}},
	}

	err := couchdb.CreateDoc(inst, &permission.Permission{
		Type:        permission.TypeShareInteract,
		Permissions: perms.Permissions,
		Codes: map[string]string{
			aliceEmail: aliceToken,
		},
		SourceID: consts.Sharings + "/" + sharingID,
	})
	require.NoError(t, err)
	err = couchdb.CreateDoc(inst, &permission.Permission{
		Type:        permission.TypeShareInteract,
		Permissions: perms.Permissions,
		Codes: map[string]string{
			bobEmail: bobToken,
		},
		SourceID: consts.Sharings + "/" + sharingID,
	})
	require.NoError(t, err)

	targetEmail := bobEmail
	targetToken := bobToken

	s := Sharing{
		SID: sharingID,
		Members: []Member{
			{Email: "owner@example.test", Status: MemberStatusOwner},
			{Email: aliceEmail, Status: MemberStatusReady},
			{Email: bobEmail, Status: MemberStatusReady},
		},
	}
	member, err := s.FindMemberByInteractCode(inst, targetToken)
	require.NoError(t, err)
	require.Equal(t, targetEmail, member.Email)
}

func TestGetInteractCodeConcurrentCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	require.NoError(t, couchdb.ResetDB(inst, consts.Permissions))

	calls := 100
	if os.Getenv("COZY_STRESS_TESTS") == "1" {
		calls = 1000
	}
	sharingID := "sharing-concurrent-interact-permissions"
	s := Sharing{
		SID:     sharingID,
		AppSlug: "drive",
		Rules: []Rule{{
			Title:   "Shared drive",
			DocType: consts.Files,
			Values:  []string{"shared-drive-root"},
		}},
	}

	members := make([]Member, calls)
	codes := make([]string, calls)
	errs := make(chan error, calls)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(calls)

	for i := 0; i < calls; i++ {
		members[i] = Member{Email: fmt.Sprintf("member-%04d@example.test", i)}
		go func(i int) {
			defer wg.Done()
			<-start
			code, err := s.GetInteractCode(inst, &members[i], i+1)
			if err != nil {
				errs <- err
				return
			}
			codes[i] = code
		}(i)
	}

	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	interact, err := permission.GetForShareInteract(inst, sharingID)
	require.NoError(t, err)
	require.Len(t, interact.Codes, calls)
	for i, member := range members {
		require.NotEmpty(t, codes[i])
		require.Equal(t, codes[i], interact.Codes[member.Email])
	}

	var perms []permission.Permission
	req := couchdb.FindRequest{
		UseIndex: "by-source-and-type",
		Selector: mango.And(
			mango.Equal("type", permission.TypeShareInteract),
			mango.Equal("source_id", consts.Sharings+"/"+sharingID),
		),
		Limit: calls,
	}
	require.NoError(t, couchdb.FindDocs(inst, consts.Permissions, &req, &perms))
	require.Len(t, perms, 1)
	require.Equal(t, permission.ShareInteractPermissionID(sharingID), perms[0].ID())
}

func TestMemberFor(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	t.Run("Owner", func(t *testing.T) {
		s := &Sharing{
			Owner: true,
			Members: []Member{
				{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
			},
		}
		m := s.MemberFor(inst)
		if assert.NotNil(t, m) {
			assert.Equal(t, MemberStatusOwner, m.Status)
		}
	})

	t.Run("OwnerNoMembers", func(t *testing.T) {
		s := &Sharing{Owner: true}
		assert.Nil(t, s.MemberFor(inst))
	})

	t.Run("RecipientByDomain", func(t *testing.T) {
		s := &Sharing{
			Owner: false,
			Members: []Member{
				{Status: MemberStatusOwner, Instance: "https://owner.cozy.tools"},
				{Status: MemberStatusReady, Instance: "https://" + inst.Domain, ReadOnly: true},
			},
		}
		m := s.MemberFor(inst)
		if assert.NotNil(t, m) {
			assert.True(t, m.ReadOnly)
		}
	})

	t.Run("NotAMember", func(t *testing.T) {
		s := &Sharing{
			Owner: false,
			Members: []Member{
				{Status: MemberStatusOwner, Instance: "https://owner.cozy.tools"},
				{Status: MemberStatusReady, Instance: "https://charlie.cozy.tools"},
			},
		}
		assert.Nil(t, s.MemberFor(inst))
	})

	t.Run("RevokedMemberSkipped", func(t *testing.T) {
		s := &Sharing{
			Owner: false,
			Members: []Member{
				{Status: MemberStatusOwner, Instance: "https://owner.cozy.tools"},
				{Status: MemberStatusRevoked, Instance: "https://" + inst.Domain, ReadOnly: false},
			},
		}
		assert.Nil(t, s.MemberFor(inst))
	})
}

func TestMemberMatching(t *testing.T) {
	s := &Sharing{Members: []Member{
		{Status: MemberStatusOwner, Name: "Alice", Email: "alice@example.net", Instance: "https://alice.example.net"},
		{Status: MemberStatusReady, Name: "Dave", Email: "dave@example.net", Instance: "https://dave.example.net"},
		{Status: MemberStatusRevoked, Name: "Eve", Email: "eve@example.net", Instance: "https://eve.example.net"},
		{Status: MemberStatusSeen, Name: "Frank", Email: "frank@example.net", Instance: "https://frank.example.net"},
	}}

	byEmail := s.MemberMatching(&Member{Email: "dave@example.net"})
	require.NotNil(t, byEmail)
	assert.Equal(t, "Dave", byEmail.Name)

	byInstance := s.MemberMatching(&Member{Instance: "https://dave.example.net"})
	require.NotNil(t, byInstance)
	assert.Equal(t, "dave@example.net", byInstance.Email)

	assert.Nil(t, s.MemberMatching(&Member{Email: "eve@example.net"}), "revoked members must not match")
	assert.Nil(t, s.MemberMatching(&Member{Email: "frank@example.net"}), "members who have not accepted the sharing must not match")
	assert.Nil(t, s.MemberMatching(&Member{Email: "alice@example.net"}), "the owner entry must not match")
	assert.Nil(t, s.MemberMatching(&Member{Instance: "https://alice.example.net"}), "the owner entry must not match")
	assert.Nil(t, s.MemberMatching(&Member{Email: "stranger@example.net"}))
	assert.Nil(t, s.MemberMatching(nil))
	assert.Nil(t, s.MemberMatching(&Member{}))
}

func TestFindContactByEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "fe" + suffix
	orgDomain := "acme-" + suffix + ".example"
	newInstance := func(domain string) *instance.Instance {
		inst, err := lifecycle.Create(&lifecycle.Options{Domain: domain, OrgDomain: orgDomain, OrgID: orgID})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
		return inst
	}
	org := newInstance(orgID + ".fe.localhost")
	alice := newInstance("alice-" + suffix + ".fe.localhost")

	bob, err := contact.Create(org, contact.CreateOptions{Email: "bob@acme.example", CozyURL: "https://bob.acme.example"})
	require.NoError(t, err)
	carol, err := contact.Create(alice, contact.CreateOptions{Email: "carol@friends.example"})
	require.NoError(t, err)

	c, created, err := FindContactByEmail(alice, "bob@acme.example")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, bob.ID(), c.ID())
	assert.Equal(t, "https://bob.acme.example", c.PrimaryCozyURL())
	_, err = contact.FindByEmail(alice, "bob@acme.example")
	assert.ErrorIs(t, err, contact.ErrNotFound)

	c, created, err = FindContactByEmail(alice, "carol@friends.example")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, carol.ID(), c.ID())

	c, created, err = FindContactByEmail(alice, "dave@unknown.example")
	require.NoError(t, err)
	assert.True(t, created)
	saved, err := contact.FindExternalWithoutCardDAVPath(alice, "dave@unknown.example")
	require.NoError(t, err, "the common feed must be able to take the created contact over")
	assert.Equal(t, c.ID(), saved.ID())

	_, _, err = FindContactByEmail(alice, "dup")
	assert.ErrorIs(t, err, ErrInvalidEmail)
	_, _, err = FindContactByEmail(alice, "Bob <bob@acme.example>")
	assert.ErrorIs(t, err, ErrInvalidEmail)
	_, err = contact.FindByEmail(alice, "dup")
	assert.ErrorIs(t, err, contact.ErrNotFound)

	// AddEmail returns the email only when it created the contact.
	s := &Sharing{Members: []Member{{Status: MemberStatusOwner}}}
	collected, err := s.AddEmail(alice, "frank@unknown.example", false)
	require.NoError(t, err)
	assert.Equal(t, "frank@unknown.example", collected)
	collected, err = s.AddEmail(alice, "carol@friends.example", false)
	require.NoError(t, err)
	assert.Empty(t, collected)

	// MembersFromEmails resolves the emails and lists the contacts it created.
	members, newContacts, err := MembersFromEmails(alice, []string{"bob@acme.example", "gina@unknown.example"}, true)
	require.NoError(t, err)
	require.Len(t, members, 2)
	assert.Equal(t, "https://bob.acme.example", members[0].Instance)
	assert.True(t, members[0].ReadOnly)
	assert.Equal(t, "gina@unknown.example", members[1].Email)
	assert.Equal(t, []string{"gina@unknown.example"}, newContacts)

	// A contact created before a failure is still listed.
	_, newContacts, err = MembersFromEmails(alice, []string{"hank@unknown.example", "dup"}, false)
	assert.ErrorIs(t, err, ErrInvalidEmail)
	assert.Equal(t, []string{"hank@unknown.example"}, newContacts)

	full := &Sharing{Members: make([]Member, maxNumberOfMembers(alice))}
	collected, err = full.AddEmail(alice, "erin@unknown.example", false)
	assert.ErrorIs(t, err, ErrTooManyMembers)
	assert.Empty(t, collected)
	_, err = contact.FindByEmail(alice, "erin@unknown.example")
	assert.ErrorIs(t, err, contact.ErrNotFound, "the contact created for a failed add must be removed")
}

type fakePublisher struct {
	requests []messaging.PublishRequest
	err      error
}

func (f *fakePublisher) Publish(_ context.Context, req messaging.PublishRequest) error {
	f.requests = append(f.requests, req)
	return f.err
}

func TestPublishCollectedContacts(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	config.GetConfig().Contexts = map[string]interface{}{
		"collected-on": map[string]interface{}{"common_contacts": true},
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	newInstance := func(name, contextName, email string) *instance.Instance {
		inst, err := lifecycle.Create(&lifecycle.Options{Domain: name + "-" + suffix + ".fe.localhost", ContextName: contextName})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
		if email != "" {
			require.NoError(t, instance.SetEmail(inst, email))
		}
		return inst
	}

	alice := newInstance("alice", "collected-on", "alice-"+suffix+"@acme.example")
	ctx := context.Background()
	address := func(card map[string]interface{}) interface{} {
		return card["emails"].(map[string]interface{})["e1"].(map[string]interface{})["address"]
	}
	// The contacts created by a request go in a single message.
	fake := &fakePublisher{}
	require.NoError(t, PublishCollectedContacts(ctx, alice, fake, []string{"dave@unknown.example", "erin@unknown.example"}))
	require.Len(t, fake.requests, 1)
	req := fake.requests[0]
	assert.Equal(t, alice.ContextName, req.ContextName)
	assert.Equal(t, rabbitmq.ExchangeContactsCollected, req.Exchange)
	assert.True(t, req.Fanout)
	assert.Empty(t, req.RoutingKey)
	msg := req.Payload.(rabbitmq.CollectedContactsMessage)
	assert.Equal(t, alice.Email, msg.UserEmail)
	require.Len(t, msg.CollectedContacts, 2)
	assert.Equal(t, "dave@unknown.example", address(msg.CollectedContacts[0]))
	assert.Equal(t, "erin@unknown.example", address(msg.CollectedContacts[1]))

	// A publish error is returned, so the job can retry.
	failing := &fakePublisher{err: errors.New("broker down")}
	err := PublishCollectedContacts(ctx, alice, failing, []string{"dave@unknown.example"})
	assert.ErrorIs(t, err, failing.err)
	err = PublishCollectedContacts(ctx, alice, nil, []string{"dave@unknown.example"})
	assert.ErrorIs(t, err, errNoCollectedPublisher)

	// Nothing is published without contacts, common contacts or an email.
	silent := &fakePublisher{}
	require.NoError(t, PublishCollectedContacts(ctx, alice, silent, nil))
	standalone := newInstance("bob", "", "bob-"+suffix+"@acme.example")
	require.NoError(t, PublishCollectedContacts(ctx, standalone, silent, []string{"erin@unknown.example"}))
	noEmail := newInstance("carol", "collected-on", "")
	require.NoError(t, PublishCollectedContacts(ctx, noEmail, silent, []string{"erin@unknown.example"}))
	assert.Empty(t, silent.requests)
}
