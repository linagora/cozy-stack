package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tarballFile struct {
	name    string
	content string
}

func setTrustedNetworks(t *testing.T, cidrs ...string) {
	t.Helper()
	config.UseTestFile(t)
	require.NoError(t, safehttp.SetTrustedPrivateNetworks(cidrs))
	t.Cleanup(func() {
		require.NoError(t, safehttp.SetTrustedPrivateNetworks(config.GetConfig().SafeHTTPTrustedNetworks))
	})
}

func TestAppSourceClientsRejectLoopback(t *testing.T) {
	setTrustedNetworks(t)
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	for name, client := range map[string]*http.Client{"tarball": &httpClient, "git manifest": ManifestClient} {
		t.Run(name, func(t *testing.T) {
			resp, err := client.Get(server.URL)
			if resp != nil {
				resp.Body.Close()
			}
			require.ErrorContains(t, err, "is not a public IP address")
		})
	}
}

// serveTarball starts a server that answers with a gzipped tarball of the
// given files, and returns its URL
func serveTarball(t *testing.T, files ...tarballFile) *url.URL {
	t.Helper()
	setTrustedNetworks(t, "127.0.0.1/32")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Size: int64(len(f.content)), Mode: 0644}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(f.content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(server.Close)
	src, err := url.Parse(server.URL + "/app.tar.gz")
	require.NoError(t, err)
	return src
}

// storedFiles returns the paths of all the files written in fs
func storedFiles(t *testing.T, fs afero.Fs) []string {
	t.Helper()
	var files []string
	err := afero.Walk(fs, "/", func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	require.NoError(t, err)
	return files
}

func TestFetchHTTPRejectsTraversingNames(t *testing.T) {
	src := serveTarball(t,
		tarballFile{name: "manifest.webapp", content: `{"slug":"app"}`},
		tarballFile{name: "../../other/1.0.0/index.html", content: "<html></html>"})
	fs := afero.NewBasePathFs(afero.NewOsFs(), t.TempDir())
	man := &WebappManifest{}
	man.SetSlug("app")
	man.SetVersion("1.0.0")

	err := fetchHTTP(src, nil, appfs.NewAferoCopier(fs), man, "")
	assert.ErrorIs(t, err, appfs.ErrInvalidFileName)
	assert.Empty(t, storedFiles(t, fs))
}

func TestFetchHTTPCopiesFiles(t *testing.T) {
	src := serveTarball(t,
		tarballFile{name: "app-1.0.0/manifest.webapp", content: `{"slug":"app"}`},
		tarballFile{name: "app-1.0.0/assets/icon.svg", content: "<svg/>"})
	fs := afero.NewBasePathFs(afero.NewOsFs(), t.TempDir())
	man := &WebappManifest{}
	man.SetSlug("app")
	man.SetVersion("1.0.0")

	err := fetchHTTP(src, nil, appfs.NewAferoCopier(fs), man, "app-1.0.0/")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"/app/1.0.0/manifest.webapp.br",
		"/app/1.0.0/assets/icon.svg.br",
	}, storedFiles(t, fs))
}

func TestHTTPFetcherIgnoresManifestChecksum(t *testing.T) {
	const registrySum = "3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855a"
	fs := afero.NewBasePathFs(afero.NewOsFs(), t.TempDir())
	fetch := func() string {
		src := serveTarball(t, tarballFile{name: "manifest.webapp", content: `{"slug":"app"}`})
		man := &WebappManifest{}
		man.SetSlug("app")
		man.SetVersion("1.0.0")
		man.SetChecksum(registrySum)
		require.NoError(t, newHTTPFetcher("manifest.webapp", nil).Fetch(src, appfs.NewAferoCopier(fs), man))
		return man.Checksum()
	}

	first, second := fetch(), fetch()
	assert.Regexp(t, "^url-[0-9a-f]{64}$", first)
	assert.NotEqual(t, first, second)
	assert.ElementsMatch(t, []string{
		"/app/1.0.0-" + first + "/manifest.webapp.br",
		"/app/1.0.0-" + second + "/manifest.webapp.br",
	}, storedFiles(t, fs))
}

func readManifest(t *testing.T, content string) (Manifest, error) {
	src := serveTarball(t, tarballFile{name: "manifest.webapp", content: content})
	inst := &Installer{
		fetcher: newHTTPFetcher(WebappManifestName, nil),
		man:     &WebappManifest{doc: &couchdb.JSONDoc{M: map[string]interface{}{}}},
		src:     src,
		slug:    "app",
	}
	return inst.ReadManifest(Installing)
}

func TestReadManifestRejectsVersionLeavingItsSegment(t *testing.T) {
	for _, version := range []string{"1.0.0-3b0c44298fc1c149/assets", ".", ".."} {
		_, err := readManifest(t, `{"slug":"app","version":"`+version+`"}`)
		assert.ErrorIs(t, err, ErrBadManifest, version)
	}
}

func TestReadManifestDropsManifestChecksum(t *testing.T) {
	man, err := readManifest(t, `{"slug":"app","version":"1.0.0","checksum":"3b0c44298fc1c149"}`)
	require.NoError(t, err)
	assert.Empty(t, man.Checksum())
}
