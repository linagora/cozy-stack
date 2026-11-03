package appfs

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listFiles returns the paths of all the files of fs
func listFiles(t *testing.T, fs afero.Fs) []string {
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

func TestCleanFileName(t *testing.T) {
	valid := map[string]string{
		"index.html":         "index.html",
		"/assets/icon.svg":   "assets/icon.svg",
		"./assets/./app.js":  "assets/app.js",
		"a..b/c.css":         "a..b/c.css",
		"assets//double.png": "assets/double.png",
	}
	for name, expected := range valid {
		clean, err := CleanFileName(name)
		assert.NoError(t, err, name)
		assert.Equal(t, expected, clean, name)
	}

	invalid := []string{
		"",
		".",
		"/",
		"..",
		"../index.html",
		"../../other/1.0.0/index.html",
		"assets/../../../other/1.0.0/index.html",
		"assets/..",
	}
	for _, name := range invalid {
		_, err := CleanFileName(name)
		assert.ErrorIs(t, err, ErrInvalidFileName, name)
	}
}

func TestAferoCopierRejectsTraversingNames(t *testing.T) {
	fs := afero.NewBasePathFs(afero.NewOsFs(), t.TempDir())
	copier := NewAferoCopier(fs)
	exists, err := copier.Start("app", "1.0.0", "abcdef")
	require.NoError(t, err)
	require.False(t, exists)

	for _, name := range []string{
		"../../other/1.0.0/index.html",
		"assets/../../../other/1.0.0/index.html",
		"..",
		"",
	} {
		info := NewFileInfo(name, 1, 0644)
		err := copier.Copy(info, strings.NewReader("x"))
		assert.ErrorIs(t, err, ErrInvalidFileName, name)
	}
	require.NoError(t, copier.Abort())
	assert.Empty(t, listFiles(t, fs))
}

func TestAferoCopierCleansNames(t *testing.T) {
	fs := afero.NewBasePathFs(afero.NewOsFs(), t.TempDir())
	copier := NewAferoCopier(fs)
	_, err := copier.Start("app", "1.0.0", "abcdef")
	require.NoError(t, err)

	for _, name := range []string{"index.html", "/assets/icon.svg", "./assets/./app.js", "a..b/c.css"} {
		info := NewFileInfo(name, 1, 0644)
		require.NoError(t, copier.Copy(info, strings.NewReader("x")), name)
	}
	require.NoError(t, copier.Commit())
	assert.ElementsMatch(t, []string{
		"/app/1.0.0-abcdef/index.html.br",
		"/app/1.0.0-abcdef/assets/icon.svg.br",
		"/app/1.0.0-abcdef/assets/app.js.br",
		"/app/1.0.0-abcdef/a..b/c.css.br",
	}, listFiles(t, fs))
}
