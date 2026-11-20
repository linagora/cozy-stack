package vfs

import (
	"errors"
	"maps"
	"os"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// LinkRoot is a folder or a file targeted by a public link, with its path on
// the instance.
type LinkRoot struct {
	ID   string
	Path string
}

// LinkRoots gives the targets of the rules of a public link, the ones given by
// ID. It is nil when the link covers the whole doctype, and empty but not nil
// when none of its targets exists anymore.
func LinkRoots(fs VFS, perms permission.Set) ([]LinkRoot, error) {
	roots := []LinkRoot{}
	for _, rule := range perms {
		if !permission.MatchType(rule, consts.Files) || rule.Selector != "" {
			continue
		}
		if len(rule.Values) == 0 {
			return nil, nil
		}
		for _, id := range rule.Values {
			p, err := PathByID(fs, id)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			roots = append(roots, LinkRoot{ID: id, Path: p})
		}
	}
	return roots, nil
}

// PathByID gives the instance path of a file or directory.
func PathByID(fs VFS, id string) (string, error) {
	dir, file, err := fs.DirOrFileByID(id)
	if err != nil {
		return "", err
	}
	if dir != nil {
		return dir.Fullpath, nil
	}
	if file == nil {
		return "", os.ErrNotExist
	}
	return file.Path(fs)
}

// RelativeToLink rewrites a path of the instance to start at the root of the
// link it is under, which keeps its name. A path under no root gives an empty
// string. Paths in the trash are kept as they are.
func RelativeToLink(roots []LinkRoot, p string) string {
	if len(roots) == 0 {
		return ""
	}
	if p == TrashDirName || strings.HasPrefix(p, TrashDirName+"/") {
		return p
	}
	for _, root := range roots {
		switch {
		case root.Path == "":
			continue
		case root.Path == "/":
			return p
		case p == root.Path || strings.HasPrefix(p, root.Path+"/"):
			return "/" + path.Base(root.Path) + strings.TrimPrefix(p, root.Path)
		}
	}
	return ""
}

// AbsoluteFromLink is the reverse of RelativeToLink: it gives the path on the
// instance of a path starting at the name of a root, or an empty string when
// it leads to none of them. Roots with the same name give the same paths, so
// such a path is refused rather than resolved in one of them.
func AbsoluteFromLink(roots []LinkRoot, p string) string {
	abs := ""
	for _, root := range roots {
		candidate := ResolvePathUnderRoot(root.Path, p)
		if candidate == "" {
			continue
		}
		if root.Path == "/" {
			return candidate
		}
		if abs != "" {
			return ""
		}
		abs = candidate
	}
	return abs
}

// RewriteEventPaths copies a file event before changing its path and restore_path.
func RewriteEventPaths(doc realtime.Doc, relative func(id, name, p string) string) realtime.Doc {
	switch d := doc.(type) {
	case *DirDoc:
		c := d.Clone().(*DirDoc)
		c.Fullpath = relative(c.DocID, c.DocName, c.Fullpath)
		c.RestorePath = relative("", "", c.RestorePath)
		return c
	case *FileDoc:
		c := d.Clone().(*FileDoc)
		c.RestorePath = relative("", "", c.RestorePath)
		return c
	case *realtime.JSONDoc:
		m := maps.Clone(d.M)
		for _, key := range []string{"path", "restore_path"} {
			p, ok := m[key].(string)
			if !ok {
				continue
			}
			id, name := "", ""
			if key == "path" {
				id = d.ID()
				name, _ = m["name"].(string)
			}
			if rel := relative(id, name, p); rel != "" {
				m[key] = rel
			} else {
				delete(m, key)
			}
		}
		return &realtime.JSONDoc{Type: d.Type, M: m}
	}
	return doc
}
