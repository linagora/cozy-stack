package sharing

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

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
