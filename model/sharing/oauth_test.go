package sharing

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/require"
)

func TestCreateSharingRequestPropagatesAccessMode(t *testing.T) {
	config.UseTestFile(t)
	inst := &instance.Instance{Domain: "alice.example.net"}

	// safehttp blocks loopback addresses outside dev mode; enable it so the
	// in-memory httptest server is reachable.
	oldBuildMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = oldBuildMode })

	newSharing := func(accessMode string) *Sharing {
		return &Sharing{
			SID:        "sharing-access-mode-test-" + accessMode,
			AppSlug:    "testapp",
			AccessMode: accessMode,
			Rules: []Rule{{
				Title:   "contacts",
				DocType: consts.Contacts,
				Values:  []string{"io.cozy.contacts"},
			}},
			Members:     []Member{{Email: "bob@example.net", Instance: "recipient"}},
			Credentials: []Credentials{{XorKey: []byte{0x01, 0x02}, State: "state-" + accessMode}},
		}
	}

	startCaptureServer := func() (*httptest.Server, chan []byte) {
		bodyCh := make(chan []byte, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodyCh <- b
			w.WriteHeader(http.StatusOK)
		}))
		return srv, bodyCh
	}

	extractAttrs := func(t *testing.T, raw []byte) map[string]interface{} {
		t.Helper()
		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &body))
		data, ok := body["data"].(map[string]interface{})
		require.True(t, ok, "missing data envelope")
		attrs, ok := data["attributes"].(map[string]interface{})
		require.True(t, ok, "missing data.attributes")
		return attrs
	}

	t.Run("limited_access is propagated to recipient", func(t *testing.T) {
		srv, bodyCh := startCaptureServer()
		defer srv.Close()
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)

		s := newSharing(AccessModeLimitedAccess)
		require.NoError(t, (&s.Members[0]).CreateSharingRequest(inst, s, &s.Credentials[0], u))

		attrs := extractAttrs(t, <-bodyCh)
		require.Equal(t, AccessModeLimitedAccess, attrs["access_mode"])
	})

	t.Run("empty access mode is omitted on the wire", func(t *testing.T) {
		srv, bodyCh := startCaptureServer()
		defer srv.Close()
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)

		s := newSharing("")
		require.NoError(t, (&s.Members[0]).CreateSharingRequest(inst, s, &s.Credentials[0], u))

		attrs := extractAttrs(t, <-bodyCh)
		_, present := attrs["access_mode"]
		require.False(t, present, "access_mode should be omitted when empty (default additive)")
	})
}
