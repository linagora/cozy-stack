package vfs

import (
	"path"
	"strings"
)

// ResolvePathUnderRoot returns the VFS path for a client path starting at the
// root's name. For example, "/Team/a" under "/Projects/Team" resolves to
// "/Projects/Team/a". An empty result means the path is outside that root.
func ResolvePathUnderRoot(root, clientPath string) string {
	if !path.IsAbs(root) || !path.IsAbs(clientPath) {
		return ""
	}
	p := path.Clean(clientPath)
	if root == "/" {
		return p
	}
	mount := "/" + path.Base(root)
	if p == mount {
		return root
	}
	if suffix, ok := strings.CutPrefix(p, mount+"/"); ok {
		return path.Join(root, suffix)
	}
	return ""
}
