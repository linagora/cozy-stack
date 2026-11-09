package sharing

import (
	"strconv"
	"testing"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareOpenFileRequestPreservesAccess(t *testing.T) {
	for _, owner := range []bool{true, false} {
		for _, ready := range []bool{true, false} {
			for _, readOnly := range []bool{true, false} {
				name := "owner=" + strconv.FormatBool(owner) + "/ready=" + strconv.FormatBool(ready) +
					"/readOnly=" + strconv.FormatBool(readOnly)
				t.Run(name, func(t *testing.T) {
					status := MemberStatusRevoked
					if ready {
						status = MemberStatusReady
					}
					opener := &FileOpener{
						Inst: &instance.Instance{Domain: "local.example"},
						File: &vfs.FileDoc{CozyMetadata: vfs.NewCozyMetadata("https://remote.example/")},
						Sharing: &Sharing{
							Owner: owner,
							Members: []Member{
								{Instance: "https://remote.example/", Status: status},
								{Instance: "https://remote.example/", Status: status},
							},
							Credentials: []Credentials{{AccessToken: &auth.AccessToken{}, InboundClientID: "caller"}},
						},
					}
					if owner {
						opener.ClientID = "caller"
					}
					prepared, err := opener.PrepareOpenFileRequest(1, readOnly)
					require.NoError(t, err)
					assert.Equal(t, 1, prepared.MemberIndex)
					assert.Equal(t, readOnly, prepared.ReadOnly)
					if ready {
						require.NotNil(t, prepared.Opts)
						assert.Equal(t, strconv.FormatBool(readOnly), prepared.Opts.Queries.Get("ReadOnly"))
						assert.Equal(t, "1", prepared.Opts.Queries.Get("MemberIndex"))
					} else {
						assert.Nil(t, prepared.Opts, "local fallback must keep the caller's access")
					}
				})
			}
		}
	}
}

func TestGetSharecodePreservesReadOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
	s := &Sharing{
		SID: "read-only-open", Owner: true, Drive: true,
		Members:     []Member{{Status: MemberStatusOwner}, {Status: MemberStatusReady, Email: "writer@example.com"}},
		Credentials: []Credentials{{InboundClientID: "caller"}},
	}
	_, err := permission.CreateSharePreviewSet(inst, s.ID(), map[string]string{"writer@example.com": "preview"},
		nil, permission.Permission{})
	require.NoError(t, err)
	for _, memberKey := range []string{"", "writer@example.com"} {
		opener := &FileOpener{Inst: inst, Sharing: s, ClientID: "caller", MemberKey: memberKey}
		code, err := opener.GetSharecode(1, true)
		require.NoError(t, err)
		assert.Equal(t, "preview", code)
	}
}
