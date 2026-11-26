package app

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cozy/cozy-stack/pkg/appfs"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitFetcherIgnoresManifestChecksum(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	const registrySum = "3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855a"

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest.webapp"), []byte(`{"slug":"app"}`), 0o644))
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"add", "manifest.webapp"},
		{"-c", "user.name=test", "-c", "user.email=test@example.org", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}

	man := &WebappManifest{}
	man.SetSlug("app")
	man.SetVersion("1.0.0")
	man.SetChecksum(registrySum)

	gitDir := t.TempDir()
	gitFs := afero.NewBasePathFs(afero.NewOsFs(), gitDir)
	fs := afero.NewBasePathFs(afero.NewOsFs(), t.TempDir())
	g := newGitFetcher("manifest.webapp", logger.WithNamespace("test"))
	err := g.doFetchWithGit(gitFs, gitDir, repo, "main", appfs.NewAferoCopier(fs), man, context.Background())
	require.NoError(t, err)

	assert.Regexp(t, "^git-[0-9a-f]{64}$", man.Checksum())
	assert.Equal(t,
		[]string{"/app/" + man.Version() + "-" + man.Checksum() + "/manifest.webapp.br"},
		storedFiles(t, fs))
}

func TestGitFetcherRejectsNonPublicHosts(t *testing.T) {
	oldMode := build.BuildMode
	build.BuildMode = build.ModeProd
	t.Cleanup(func() { build.BuildMode = oldMode })

	g := newGitFetcher("manifest.webapp", logger.WithNamespace("test"))
	man := &WebappManifest{}
	man.SetSlug("app")
	fs := appfs.NewAferoCopier(afero.NewBasePathFs(afero.NewOsFs(), t.TempDir()))

	for _, raw := range []string{
		"git+ssh://127.0.0.1/app.git",
		"git://10.0.0.5/app.git",
		"git+https://192.168.0.1/app.git",
		"git+ssh://169.254.169.254/app.git",
	} {
		src, err := url.Parse(raw)
		require.NoError(t, err)

		if isGitSSHScheme(src.Scheme) {
			_, err = g.FetchManifest(src)
			require.Error(t, err, raw)
			assert.Contains(t, err.Error(), "not a ", raw)
		}

		err = g.Fetch(src, fs, man)
		require.Error(t, err, raw)
		assert.Contains(t, err.Error(), "not a ", raw)
	}
}
