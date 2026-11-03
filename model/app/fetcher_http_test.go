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
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tarballFile struct {
	name    string
	content string
}

// serveTarball starts a server that answers with a gzipped tarball of the
// given files, and returns its URL
func serveTarball(t *testing.T, files ...tarballFile) *url.URL {
	t.Helper()
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
	config.UseTestFile(t)
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
	config.UseTestFile(t)
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
