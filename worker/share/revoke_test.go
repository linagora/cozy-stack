package share

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRevokeQueuesRecipientBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	previousMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = previousMode })
	testutils.NeedCouchdb(t)
	for _, drive := range []bool{false, true} {
		t.Run(fmt.Sprintf("drive=%t", drive), func(t *testing.T) {
			inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
			gate := make(chan struct{})
			var release sync.Once
			var delivered atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-gate
				assert.Equal(t, http.MethodDelete, r.Method)
				assert.NotEmpty(t, r.Header.Get("Authorization"))
				delivered.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			defer release.Do(func() { close(gate) })
			dir, err := vfs.Mkdir(inst.VFS(), "/Shared", nil)
			require.NoError(t, err)
			s, err := sharing.CreateDrive(inst, dir.ID(), "Shared", "")
			require.NoError(t, err)
			_, err = s.Create(inst)
			require.NoError(t, err)
			s.Drive = drive
			codes := make(map[string]string)
			for i := 0; i < 51; i++ {
				email := fmt.Sprintf("member%d@example.net", i)
				token := fmt.Sprintf("token%d", i)
				codes[email] = token
				s.Members = append(s.Members, sharing.Member{Instance: srv.URL, Email: email, Status: sharing.MemberStatusReady})
				s.Credentials = append(s.Credentials, sharing.Credentials{
					Client:      &auth.Client{ClientID: "remote-client"},
					AccessToken: &auth.AccessToken{AccessToken: token},
				})
			}
			inbound, err := sharing.CreateOAuthClient(inst, &s.Members[1])
			require.NoError(t, err)
			s.Credentials[0].InboundClientID = inbound.ClientID
			require.NoError(t, couchdb.UpdateDoc(inst, s))
			_, err = permission.CreateShareInteractSet(inst, s.SID, codes, permission.Permission{Permissions: s.CreateInteractSet()})
			require.NoError(t, err)
			if drive {
				// Resume cleanup after a previous attempt expired owner permissions.
				s.Active = false
				require.NoError(t, couchdb.UpdateDoc(inst, s))
				require.NoError(t, s.RevokeInteractPermissions(inst))
			}
			done := make(chan error, 1)
			go func() { done <- s.Revoke(inst) }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("owner revocation waited for recipient HTTP responses")
			}
			assert.Zero(t, delivered.Load())
			assert.False(t, s.Active)
			for i := 1; i < len(s.Members); i++ {
				assert.Equal(t, sharing.MemberStatusRevoked, s.Members[i].Status)
				assert.Equal(t, sharing.Credentials{}, s.Credentials[i-1])
			}
			_, err = oauth.FindClient(inst, inbound.ClientID)
			require.True(t, couchdb.IsNotFoundError(err))
			stored, err := sharing.FindSharing(inst, s.SID)
			if drive {
				require.True(t, couchdb.IsNotFoundError(err))
			} else {
				require.NoError(t, err)
				assert.False(t, stored.Active)
			}
			root, err := inst.VFS().DirByID(dir.ID())
			require.NoError(t, err)
			assert.NotContains(t, root.ReferencedBy, s.DocReference())
			interact, err := permission.GetPermissionByIDIncludingExpired(inst, permission.ShareInteractPermissionID(s.SID))
			require.NoError(t, err)
			require.NotNil(t, interact.ExpiresAt)
			assert.True(t, interact.Expired())
			var jobs []job.Job
			require.NoError(t, couchdb.GetAllDocs(inst, consts.Jobs, &couchdb.AllDocsRequest{}, &jobs))
			var sizes []int
			for _, j := range jobs {
				if j.WorkerType != sharing.RevocationWorker {
					continue
				}
				var msgJob sharing.RevocationJob
				require.NoError(t, j.Message.Unmarshal(&msgJob))
				assert.NotContains(t, string(j.Message), "remote-client")
				data, err := account.DecryptCredentialsData(msgJob.Encrypted)
				require.NoError(t, err)
				payload := data.(map[string]interface{})
				sizes = append(sizes, len(payload["recipients"].([]interface{})))
			}
			assert.ElementsMatch(t, []int{50, 1}, sizes)
			release.Do(func() { close(gate) })
			require.Eventually(t, func() bool { return delivered.Load() == 51 }, 5*time.Second, 10*time.Millisecond)
			for _, j := range jobs {
				if j.WorkerType == sharing.RevocationWorker {
					require.Eventually(t, func() bool {
						current, err := job.Get(inst, j.ID())
						return err == nil && current.State == job.Done
					}, 5*time.Second, 10*time.Millisecond)
				}
			}
		})
	}
}

func TestRevocationNotificationRetriesWithoutOwnerDocument(t *testing.T) {
	config.UseTestFile(t)
	previousMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = previousMode })
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	msg := &sharing.RevocationMsg{SharingID: "already-deleted", Recipients: []sharing.RevocationRecipient{{
		Member: sharing.Member{Instance: srv.URL, Status: sharing.MemberStatusReady}, Token: "drive-token",
	}}}
	require.Error(t, sharing.NotifyRevocations(context.Background(), nil, msg))
	require.NoError(t, sharing.NotifyRevocations(context.Background(), nil, msg))
	require.NoError(t, sharing.NotifyRevocations(context.Background(), nil, msg))
}

func TestRevocationRefreshDoesNotWriteOwnerCredentials(t *testing.T) {
	config.UseTestFile(t)
	previousMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = previousMode })
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/access_token" {
			refreshes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"renewed","refresh_token":"refresh"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer renewed" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	msg := &sharing.RevocationMsg{SharingID: "deleted-owner-sharing", Recipients: []sharing.RevocationRecipient{{
		Member: sharing.Member{Instance: srv.URL, Status: sharing.MemberStatusReady}, Token: "expired",
		Credentials: sharing.Credentials{
			Client:      &auth.Client{ClientID: "client", ClientSecret: "secret"},
			AccessToken: &auth.AccessToken{AccessToken: "expired", RefreshToken: "refresh"},
		},
	}}}
	// There is no owner instance or document to update after refreshing.
	require.NoError(t, sharing.NotifyRevocations(context.Background(), nil, msg))
	assert.EqualValues(t, 1, refreshes.Load())
}

type revocationTransport func(*http.Request) (*http.Response, error)

func (f revocationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRevokePendingDriveWithoutNotificationCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
	s := &sharing.Sharing{Owner: true, Active: true, Drive: true, OrgDrive: true,
		Members: []sharing.Member{{Status: sharing.MemberStatusOwner}, {
			Instance: "https://pending.example", Status: sharing.MemberStatusPendingInvitation,
		}},
		Credentials: []sharing.Credentials{{}},
	}
	require.NoError(t, couchdb.CreateDoc(inst, s))
	require.NoError(t, s.Revoke(inst))
	assert.False(t, s.Active)
	assert.Equal(t, sharing.MemberStatusRevoked, s.Members[1].Status)
}

func TestRevokeQueueFailureCanBeRetried(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	previousMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = previousMode })
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
	var delivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s := &sharing.Sharing{Owner: true, Active: true,
		Members:     []sharing.Member{{Status: sharing.MemberStatusOwner}, {Instance: srv.URL, Status: sharing.MemberStatusReady}},
		Credentials: []sharing.Credentials{{Client: &auth.Client{ClientID: "remote"}, AccessToken: &auth.AccessToken{AccessToken: "token"}}},
	}
	require.NoError(t, couchdb.CreateDoc(inst, s))
	client := config.CouchClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	client.Transport = revocationTransport(func(r *http.Request) (*http.Response, error) {
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.Contains(r.URL.Path, couchdb.EscapeCouchdbName(consts.Jobs)) {
			return nil, errors.New("job persistence unavailable")
		}
		return original.RoundTrip(r)
	})
	err := s.Revoke(inst)
	client.Transport = original
	require.ErrorContains(t, err, "job persistence unavailable")
	stored, err := sharing.FindSharing(inst, s.SID)
	require.NoError(t, err)
	assert.False(t, stored.Active)
	assert.NotNil(t, stored.Credentials[0].AccessToken)
	assert.Zero(t, delivered.Load())
	require.NoError(t, stored.Revoke(inst))
	require.Eventually(t, func() bool { return delivered.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, stored.Revoke(inst))
}
