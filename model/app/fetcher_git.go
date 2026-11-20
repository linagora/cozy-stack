package app

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/spf13/afero"
)

var cloneTimeout = 30 * time.Second

const (
	ghRawManifestURL = "https://raw.githubusercontent.com/%s/%s/%s/%s"
	glRawManifestURL = "https://%s/%s/%s/raw/%s/%s"
)

var (
	// ghURLRegex is used to identify github
	ghURLRegex = regexp.MustCompile(`/([^/]+)/([^/]+).git`)
	// glURLRegex is used to identify gitlab
	glURLRegex = regexp.MustCompile(`/(.+)/([^/]+).git`)
)

type gitFetcher struct {
	manFilename string
	log         logger.Logger
}

func newGitFetcher(manFilename string, log logger.Logger) *gitFetcher {
	return &gitFetcher{
		manFilename: manFilename,
		log:         log,
	}
}

// ManifestClient is the client used to HTTP resources from the git fetcher. It
// is exported for tests purposes only.
var ManifestClient = &http.Client{
	Timeout:   60 * time.Second,
	Transport: safehttp.ClientWithKeepAlive.Transport,
}

func isGithub(src *url.URL) bool {
	return src.Host == "github.com"
}

func isGitlab(src *url.URL) bool {
	return src.Host == "framagit.org" ||
		src.Host == "forge.grandlyon.com" ||
		strings.Contains(src.Host, "gitlab")
}

func (g *gitFetcher) FetchManifest(src *url.URL) (r io.ReadCloser, err error) {
	defer func() {
		if err != nil {
			g.log.Errorf("Error while fetching app manifest %s: %s",
				src.String(), err.Error())
		}
	}()

	if isGitSSHScheme(src.Scheme) {
		return g.fetchManifestFromGitArchive(src)
	}

	var u string
	if isGithub(src) {
		u, err = resolveGithubURL(src, g.manFilename)
	} else if isGitlab(src) {
		u, err = resolveGitlabURL(src, g.manFilename)
	} else {
		u, err = resolveManifestURL(src, g.manFilename)
	}
	if err != nil {
		return nil, err
	}

	g.log.Infof("Fetching manifest on %s", u)
	res, err := ManifestClient.Get(u)
	if err != nil || res.StatusCode != 200 {
		g.log.Errorf("Error while fetching manifest on %s", u)
		return nil, ErrManifestNotReachable
	}

	return res.Body, nil
}

// Use the git archive method to download a manifest from the git repository.
func (g *gitFetcher) fetchManifestFromGitArchive(src *url.URL) (io.ReadCloser, error) {
	var branch string
	src, branch = getRemoteURL(src)
	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()

	if branch == "" {
		branch = "main"
		handle, err := g.doFetchManifestFromGitArchive(src, branch, ctx)
		if err == nil {
			return handle, nil
		}
		branch = "master"
	}
	return g.doFetchManifestFromGitArchive(src, branch, ctx)
}

func (g *gitFetcher) doFetchManifestFromGitArchive(src *url.URL, branch string, ctx context.Context) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "git",
		"archive",
		"--remote", src.String(),
		fmt.Sprintf("refs/heads/%s", branch),
		g.manFilename)
	g.log.Infof("Fetching manifest %s", strings.Join(cmd.Args, " "))
	stdout, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrNotSupportedSource
		}
		return nil, ErrManifestNotReachable
	}
	buf := new(bytes.Buffer)
	r := tar.NewReader(bytes.NewReader(stdout))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ErrManifestNotReachable
		}
		if h.Name != g.manFilename {
			continue
		}
		if _, err = io.Copy(buf, r); err != nil {
			return nil, ErrManifestNotReachable
		}
		return io.NopCloser(buf), nil
	}
	return nil, ErrManifestNotReachable
}

func (g *gitFetcher) Fetch(src *url.URL, fs appfs.Copier, man Manifest) (err error) {
	defer func() {
		if err != nil {
			g.log.Errorf("Error while fetching or copying repository %s: %s",
				src.String(), err.Error())
		}
	}()

	osFs := afero.NewOsFs()
	gitDir, err := afero.TempDir(osFs, "", "cozy-app-"+man.Slug())
	if err != nil {
		return err
	}
	defer func() { _ = osFs.RemoveAll(gitDir) }()

	gitFs := afero.NewBasePathFs(osFs, gitDir)
	if src.Scheme == "git+https" {
		src.Scheme = "https"
	}
	// XXX Gitlab doesn't support the git protocol
	if src.Scheme == "git" && isGitlab(src) {
		src.Scheme = "https"
	}

	// If the scheme uses ssh, we have to use the git command.
	if isGitSSHScheme(src.Scheme) {
		err = g.fetchWithGit(gitFs, gitDir, src, fs, man)
		if errors.Is(err, exec.ErrNotFound) {
			return ErrNotSupportedSource
		}
		return err
	}

	return g.fetchWithGit(gitFs, gitDir, src, fs, man)
}

func (g *gitFetcher) fetchWithGit(gitFs afero.Fs, gitDir string, src *url.URL, fs appfs.Copier, man Manifest) (err error) {
	var branch string
	src, branch = getRemoteURL(src)
	srcStr := src.String()

	// GitHub doesn't accept git ls-remote with unencrypted git protocol.
	// Cf https://github.blog/2021-09-01-improving-git-protocol-security-github/
	if isGithub(src) && src.Scheme == "git" {
		srcStr = strings.Replace(srcStr, "git", "https", 1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()

	if branch == "" {
		branch = "main"
		err := g.doFetchWithGit(gitFs, gitDir, srcStr, branch, fs, man, ctx)
		if err == nil {
			return nil
		}
		branch = "master"
	}
	return g.doFetchWithGit(gitFs, gitDir, srcStr, branch, fs, man, ctx)
}

func (g *gitFetcher) doFetchWithGit(
	gitFs afero.Fs,
	gitDir, srcStr, branch string,
	fs appfs.Copier,
	man Manifest,
	ctx context.Context,
) (err error) {
	// The first command we execute is a ls-remote to check the last commit from
	// the remote branch and see if we already have a checked-out version of this
	// tree.
	cmd := exec.CommandContext(ctx, "git",
		"ls-remote", "--quiet",
		srcStr, fmt.Sprintf("refs/heads/%s", branch))
	lsRemote, err := cmd.Output()
	if err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			g.log.Errorf("ls-remote error of %s: %s",
				strings.Join(cmd.Args, " "), err.Error())
		}
		return err
	}

	lsRemoteFields := bytes.Fields(lsRemote)
	if len(lsRemoteFields) == 0 {
		return fmt.Errorf("git: unexpected ls-remote output")
	}

	slug := man.Slug()
	version := man.Version() + "-" + string(lsRemoteFields[0])

	// The git fetcher needs to update the actual version of the application to
	// reflect the git version of the repository.
	man.SetVersion(version)
	man.SetChecksum(sourceChecksum("git", srcStr))

	// If the application folder already exists, we can bail early.
	exists, err := fs.Start(slug, version, man.Checksum())
	if err != nil || exists {
		return err
	}
	defer func() {
		if err != nil {
			_ = fs.Abort()
		} else {
			err = fs.Commit()
		}
	}()

	cmd = exec.CommandContext(ctx, "git",
		"clone",
		"--quiet",
		"--depth", "1",
		"--single-branch",
		"--branch", branch,
		"--", srcStr, gitDir)

	g.log.Infof("Clone with git: %s", strings.Join(cmd.Args, " "))
	stdoutStderr, err := cmd.CombinedOutput()
	if err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			g.log.Errorf("Clone error of %s %s: %s", srcStr, stdoutStderr,
				err.Error())
		}
		return err
	}

	return afero.Walk(gitFs, "/", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		src, err := gitFs.Open(path)
		if err != nil {
			return err
		}
		fileinfo := appfs.NewFileInfo(path, info.Size(), info.Mode())
		return fs.Copy(fileinfo, src)
	})
}

func getWebBranch(src *url.URL) string {
	if src.Fragment != "" {
		return src.Fragment
	}
	return "HEAD"
}

func getRemoteURL(src *url.URL) (*url.URL, string) {
	branch := src.Fragment
	clonedSrc := *src
	clonedSrc.Fragment = ""
	return &clonedSrc, branch
}

func resolveGithubURL(src *url.URL, filename string) (string, error) {
	match := ghURLRegex.FindStringSubmatch(src.Path)
	if len(match) != 3 {
		return "", &url.Error{
			Op:  "parsepath",
			URL: src.String(),
			Err: errors.New("Could not parse url git path"),
		}
	}

	user, project := match[1], match[2]
	branch := getWebBranch(src)

	u := fmt.Sprintf(ghRawManifestURL, user, project, branch, filename)
	return u, nil
}

func resolveGitlabURL(src *url.URL, filename string) (string, error) {
	match := glURLRegex.FindStringSubmatch(src.Path)
	if len(match) != 3 {
		return "", &url.Error{
			Op:  "parsepath",
			URL: src.String(),
			Err: errors.New("Could not parse url git path"),
		}
	}

	user, project := match[1], match[2]
	branch := getWebBranch(src)

	u := fmt.Sprintf(glRawManifestURL, src.Host, user, project, branch, filename)
	return u, nil
}

func resolveManifestURL(src *url.URL, filename string) (string, error) {
	srccopy, _ := url.Parse(src.String())
	srccopy.Scheme = "https"
	if srccopy.Path == "" || srccopy.Path[len(srccopy.Path)-1] != '/' {
		srccopy.Path += "/"
	}
	srccopy.Path += filename
	return srccopy.String(), nil
}

func isGitSSHScheme(scheme string) bool {
	return scheme == "git+ssh" || scheme == "ssh+git"
}

var _ Fetcher = &gitFetcher{}
