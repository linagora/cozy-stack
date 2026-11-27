package realtime

import (
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// linkPaths rewrites the paths of the documents sent to the holder of a public
// link, to start at the roots of the link. It is used by the goroutine that
// writes the events only.
type linkPaths struct {
	inst  *instance.Instance
	perm  *permission.Permission
	roots []vfs.LinkRoot
	fresh bool // the roots have been read for the current event
}

func newLinkPaths(inst *instance.Instance, perm *permission.Permission) (*linkPaths, error) {
	scope, err := sharing.ResolveFilePaths(inst, perm)
	if err != nil {
		return nil, err
	}
	return &linkPaths{inst: inst, perm: perm, roots: scope.Roots}, nil
}

// restricted tells if the paths must be rewritten for this token: a link
// whose targets are all gone still hides the paths.
func (l *linkPaths) restricted() bool {
	return l.roots != nil
}

// rewrite gives a copy of the document where the paths start at a root of the
// link, and are left out when they lead elsewhere.
func (l *linkPaths) rewrite(doc realtime.Doc) realtime.Doc {
	// Another doctype can have its own fields named path and restore_path
	if doc.DocType() != consts.Files {
		return doc
	}
	l.fresh = false
	return vfs.RewriteEventPaths(doc, l.relative)
}

// relative formats p like the HTTP routes, and leaves it out when it is under
// none of the roots, the trash included: the watched documents may have left
// the link.
func (l *linkPaths) relative(id, _, p string) string {
	if p == "" || len(l.roots) == 0 {
		return ""
	}
	if !placed(l.roots, id, p) {
		// A root, or a folder above one, may have been renamed, moved or trashed
		if l.fresh {
			return ""
		}
		scope, err := sharing.ResolveFilePaths(l.inst, l.perm)
		if err != nil {
			return ""
		}
		l.roots, l.fresh = scope.Roots, true
		if !placed(l.roots, id, p) {
			return ""
		}
	}
	return vfs.RelativeToLink(l.roots, p)
}

// placed tells if the roots are up to date for p: the root with this ID is at
// p, and p is under one of the roots.
func placed(roots []vfs.LinkRoot, id, p string) bool {
	under := false
	for _, root := range roots {
		if root.ID == id && root.Path != p {
			return false
		}
		under = under || p == root.Path || strings.HasPrefix(p, root.Path+"/")
	}
	return under
}
