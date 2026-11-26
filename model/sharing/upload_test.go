package sharing

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/model/vfs"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadFileDoesNotFollowRedirectsToUnspecifiedAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	// Loopback is reachable in dev mode, 0.0.0.0 is never accepted by safehttp.
	oldBuildMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = oldBuildMode })

	var hits atomic.Int32
	canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(canary.Close)
	port := strconv.Itoa(canary.Listener.Addr().(*net.TCPAddr).Port)

	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/metadata") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"abc"}`))
			return
		}
		http.Redirect(w, r, "http://0.0.0.0:"+port+"/", http.StatusFound)
	}))
	t.Cleanup(member.Close)

	fs := inst.VFS()
	doc, err := vfs.NewFileDoc("upload.txt", consts.RootDirID, -1, nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := fs.CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = f.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	fileDoc := couchdb.JSONDoc{Type: consts.Files}
	require.NoError(t, couchdb.GetDoc(inst, consts.Files, doc.ID(), &fileDoc))
	file := fileDoc.M

	s := &Sharing{
		SID:   "upload-redirect-test",
		Owner: true,
		Rules: []Rule{{Title: "files", DocType: consts.Files, Values: []string{doc.ID()}}},
		Members: []Member{
			{Status: MemberStatusOwner, Name: "Alice", Email: "alice@example.net"},
			{Status: MemberStatusReady, Name: "Bob", Email: "bob@example.net", Instance: member.URL},
		},
		Credentials: []Credentials{{
			XorKey:      []byte{0x01, 0x02},
			AccessToken: &auth.AccessToken{AccessToken: "token"},
		}},
	}

	err = s.uploadFile(inst, &s.Members[1], file, 0)
	assert.Error(t, err)
	assert.Zero(t, hits.Load())
}
