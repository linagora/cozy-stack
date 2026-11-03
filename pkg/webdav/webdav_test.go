package webdav

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSkipsNonChildHrefs(t *testing.T) {
	// safehttp refuses loopback hosts outside dev mode.
	oldBuildMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = oldBuildMode })

	const dir = "/files/alice/Documents"
	hrefs := []struct {
		name string
		href string
		want bool
	}{
		{"child file", "/remote.php/dav" + dir + "/a.txt", true},
		{"child directory", "/remote.php/dav" + dir + "/sub/", true},
		{"child with escaped name", "/remote.php/dav" + dir + "/my%20file.txt", true},
		{"directory itself", "/remote.php/dav" + dir + "/", false},
		{"directory itself without slash", "/remote.php/dav" + dir, false},
		{"directory itself with dot", "/remote.php/dav" + dir + "/./", false},
		{"empty href", "", false},
		{"double slash", "/remote.php/dav//", false},
		{"dot parent", "/remote.php/dav/files/./", false},
		{"parent directory", "/remote.php/dav/files/alice/", false},
		{"grandchild", "/remote.php/dav" + dir + "/sub/b.txt", false},
	}

	for _, tc := range hrefs {
		t.Run(tc.name, func(t *testing.T) {
			body := `<d:multistatus xmlns:d="DAV:"><d:response><d:href>` + tc.href +
				`</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>` +
				`<d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(207)
				_, _ = fmt.Fprint(w, body)
			}))
			t.Cleanup(srv.Close)

			u, err := url.Parse(srv.URL)
			require.NoError(t, err)
			client := &Client{
				Scheme:   u.Scheme,
				Host:     u.Host,
				Username: "alice",
				Password: "secret",
				BasePath: "/remote.php/dav",
				Logger:   logger.WithNamespace("webdav-test"),
			}
			items, err := client.List(dir)
			require.NoError(t, err)
			if tc.want {
				assert.Len(t, items, 1)
			} else {
				assert.Empty(t, items)
			}
		})
	}
}
