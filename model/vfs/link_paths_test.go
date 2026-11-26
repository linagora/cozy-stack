package vfs

import (
	"errors"
	"os"
	"testing"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinkPaths(t *testing.T) {
	roots := []LinkRoot{{ID: "docs", Path: "/Private/Clients/Docs"}}

	for p, expected := range map[string]string{
		"/Private/Clients/Docs":      "/Docs",
		"/Private/Clients/Docs/Sub":  "/Docs/Sub",
		"/Private/Clients/DocsOther": "",
		"/Private/Clients":           "",
		"/Private":                   "",
		"/.cozy_trash/Sub":           "/.cozy_trash/Sub",
		"":                           "",
	} {
		assert.Equal(t, expected, RelativeToLink(roots, p), p)
	}

	for p, expected := range map[string]string{
		"/Docs":                       "/Private/Clients/Docs",
		"/Docs/Sub/":                  "/Private/Clients/Docs/Sub",
		"/Docs/../Other":              "",
		"/Docs/../../Docs":            "/Private/Clients/Docs",
		"/Private/Clients/Docs":       "",
		"/DocsOther":                  "",
		"Docs":                        "",
		"/":                           "",
		"/Docs/Sub/../../../Clients/": "",
	} {
		assert.Equal(t, expected, AbsoluteFromLink(roots, p), p)
	}

	whole := []LinkRoot{{ID: "root", Path: "/"}}
	assert.Equal(t, "/Private", RelativeToLink(whole, "/Private"))
	assert.Equal(t, "/Private", AbsoluteFromLink(whole, "/Private"))

	trashed := []LinkRoot{{ID: "docs", Path: "/.cozy_trash/Private/Clients/Docs"}}
	assert.Equal(t, "/.cozy_trash/Docs", RelativeToLink(trashed, "/.cozy_trash/Private/Clients/Docs"))
	assert.Equal(t, "/.cozy_trash/Docs/a.txt", RelativeToLink(trashed, "/.cozy_trash/Private/Clients/Docs/a.txt"))
	assert.Equal(t, "/.cozy_trash/Private/Clients/Docs/a.txt", AbsoluteFromLink(trashed, "/.cozy_trash/Docs/a.txt"))

	repeated := []LinkRoot{{ID: "docs", Path: "/Private/Docs"}, {ID: "docs", Path: "/Private/Docs"}}
	assert.Equal(t, "/Private/Docs/Sub", AbsoluteFromLink(repeated, "/Docs/Sub"))

	homonyms := []LinkRoot{{ID: "a", Path: "/A/Docs"}, {ID: "b", Path: "/B/Docs"}, {ID: "c", Path: "/C/Other"}}
	assert.Empty(t, AbsoluteFromLink(homonyms, "/Docs"))
	assert.Empty(t, AbsoluteFromLink(homonyms, "/Docs/Sub"))
	assert.Equal(t, "/C/Other/Sub", AbsoluteFromLink(homonyms, "/Other/Sub"))
}

type lookupVFS struct {
	VFS
	dirs map[string]*DirDoc
	err  error
}

func (fs lookupVFS) DirOrFileByID(id string) (*DirDoc, *FileDoc, error) {
	return fs.dirs[id], nil, fs.err
}

func TestLinkRoots(t *testing.T) {
	byID := permission.Set{permission.Rule{Type: consts.Files, Values: []string{"gone"}}}

	_, err := LinkRoots(lookupVFS{err: errors.New("couchdb is down")}, byID)
	assert.Error(t, err)

	roots, err := LinkRoots(lookupVFS{err: os.ErrNotExist}, byID)
	require.NoError(t, err)
	assert.NotNil(t, roots, "a link whose targets are gone is still restricted")
	assert.Empty(t, roots)
	assert.Empty(t, RelativeToLink(roots, "/Private"))
	assert.Empty(t, AbsoluteFromLink(roots, "/Private"))

	whole := permission.Set{permission.Rule{Type: consts.Files}}
	roots, err = LinkRoots(lookupVFS{}, whole)
	require.NoError(t, err)
	assert.Nil(t, roots)

	readOneWriteAll := permission.Set{
		permission.Rule{Type: consts.Files, Verbs: permission.Verbs(permission.POST)},
		permission.Rule{Type: consts.Files, Verbs: permission.Verbs(permission.GET), Values: []string{"docs"}},
	}
	dirs := map[string]*DirDoc{"docs": {Fullpath: "/Private/Docs"}, "root": {Fullpath: "/"}}
	roots, err = LinkRoots(lookupVFS{dirs: dirs}, readOneWriteAll)
	require.NoError(t, err)
	assert.Equal(t, []LinkRoot{{ID: "docs", Path: "/Private/Docs"}}, roots)

	// The root of the instance must not be shadowed by another root, whatever the order
	for _, values := range [][]string{{"docs", "root"}, {"root", "docs"}} {
		roots, err = LinkRoots(lookupVFS{dirs: dirs}, permission.Set{permission.Rule{Type: consts.Files, Values: values}})
		require.NoError(t, err)
		assert.Nil(t, roots, values)
	}
}
