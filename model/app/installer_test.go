package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path"
	"strconv"
	"testing"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/stretchr/testify/require"
)

var stackStarted bool

var localGitCmd *exec.Cmd
var localGitDir string
var localVersion string
var localServices string
var ts *httptest.Server

var manGen func() string
var manName string

type transport struct{}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := new(http.Request)
	*req2 = *req
	req2.URL, _ = url.Parse(ts.URL)
	return http.DefaultTransport.RoundTrip(req2)
}

func manifestWebapp() string {
	if localServices == "" {
		localServices = "{}"
	}
	return `{
  "description": "A mini app to test cozy-stack-v2",
  "developer": {
    "name": "Bruno",
    "url": "cozy.io"
  },
  "license": "MIT",
  "name": "mini-app",
  "permissions": {},
  "slug": "mini",
  "type": "webapp",
  "version": "` + localVersion + `",
  "services": ` + localServices + `
}`
}

func manifestKonnector() string {
	return `{
  "description": "A mini konnector to test cozy-stack-v2",
  "type": "node",
  "developer": {
    "name": "Bruno",
    "url": "cozy.io"
  },
  "license": "MIT",
  "name": "mini-app",
  "permissions": {},
  "slug": "mini",
  "type": "konnector",
  "version": "` + localVersion + `"
}`
}

func storedDir(man app.Manifest) string {
	return path.Join("/", man.Slug(), man.Version()+"-"+man.Checksum())
}

func serveGitRep(t *testing.T) (string, context.CancelFunc) {
	localGitDir = t.TempDir()
	args := `
echo '` + manifestWebapp() + `' > ` + app.WebappManifestName + ` && \
echo '` + manifestKonnector() + `' > ` + app.KonnectorManifestName + ` && \
git init . && \
git config user.name "cozy" && \
git config user.email "cozy@cloud.fr" && \
git add . && \
git commit -m 'Initial commit' && \
git checkout -b branch && \
echo 'branch' > branch && \
git add . && \
git commit -m 'Create a branch' && \
git checkout -`

	cmd := exec.Command("bash", "-c", args)
	cmd.Dir = localGitDir

	out1, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to setup the git repo (output: %q): %s", out1, err)

	port := findAvailablePort(t)

	// "git daemon --reuseaddr --base-path=./ --export-all ./.git"
	ctx, cancel := context.WithCancel(context.Background())
	localGitCmd = exec.CommandContext(ctx,
		"git", "daemon", "--reuseaddr", "--port="+port, "--base-path=./", "--export-all", "./.git")
	localGitCmd.Dir = localGitDir

	var out2 bytes.Buffer
	localGitCmd.Stdout = &out2
	localGitCmd.Stderr = &out2

	go func() {
		err = localGitCmd.Run()
		if err != nil && !errors.Is(context.Canceled, ctx.Err()) {
			panic(fmt.Sprintf("failed to run the git server (output: %q): %s", out2.String(), err))
		}
	}()

	return fmt.Sprintf("git://localhost:%s/", port), cancel
}

func doUpgrade(t *testing.T, major int) {
	localVersion = fmt.Sprintf("%d.0.0", major)
	args := `
echo '` + manifestWebapp() + `' > ` + app.WebappManifestName + ` && \
echo '` + manifestKonnector() + `' > ` + app.KonnectorManifestName + ` && \
git commit -am "Upgrade commit" && \
git checkout branch && \
git rebase master && \
git checkout master`
	cmd := exec.Command("bash", "-c", args)
	cmd.Dir = localGitDir
	if out, err := cmd.Output(); err != nil {
		t.Log(string(out), err)
	} else {
		t.Log("did upgrade", localVersion)
	}
}

func findAvailablePort(t *testing.T) string {
	t.Helper()

	// ":0" means: find my any available port
	l, err := net.Listen("tcp", ":0")
	require.NoError(t, err)

	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	return strconv.Itoa(port)
}
