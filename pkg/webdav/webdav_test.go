package webdav

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListKeepsOnlyDirectChildren(t *testing.T) {
	// safehttp refuses loopback hosts outside dev mode.
	oldBuildMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = oldBuildMode })

	hrefs := []string{
		"/remote.php/dav/files/u/Documents/",          // the directory itself
		"/remote.php/dav/files/u/Documents/a%20b.txt", // a file
		"/remote.php/dav/files/u/Documents/sub/",      // a sub-directory
		"",                                            // empty
		"/remote.php/dav/files/./",                    // a parent
		"/remote.php/dav//",                           // the root
		"/remote.php/dav/files/u/Documents/./",        // the directory itself, again
		"/remote.php/dav/files/u/Documents/sub/..",    // the directory itself, again
		"/remote.php/dav/files/u/Documents/sub/c.txt", // not a direct child
		"/remote.php/dav/files/u/Other/d.txt",         // not a child
		"/remote.php/dav/files/u/Documents/sub/../..", // a parent
		"/remote.php/dav/files/u/Documents/%2E%2E/e",  // not a child once unescaped
	}
	var body strings.Builder
	body.WriteString(`<d:multistatus xmlns:d="DAV:">`)
	for _, href := range hrefs {
		fmt.Fprintf(&body, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getcontentlength>1</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href)
	}
	body.WriteString(`</d:multistatus>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, body.String())
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	c := &Client{Scheme: u.Scheme, Host: u.Host, BasePath: "/remote.php/dav", Logger: logger.WithNamespace("webdav-test")}
	items, err := c.List("/files/u/Documents")
	require.NoError(t, err)

	var got []string
	for _, item := range items {
		got = append(got, item.Href)
	}
	assert.ElementsMatch(t, []string{"/files/u/Documents/a b.txt", "/files/u/Documents/sub"}, got)
}
