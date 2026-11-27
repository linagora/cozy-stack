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
	read  bool  // the roots have been read again for the current event
	err   error // the error of that read
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
	l.read, l.err = false, nil
	return vfs.RewriteEventPaths(doc, l.relative)
}

// relative formats p like the HTTP routes, and leaves it out when it is under
// none of the roots, the trash included: the watched documents may have left
// the link.
func (l *linkPaths) relative(_, _, p string) string {
	if p == "" || len(l.roots) == 0 {
		return ""
	}
	// Under the first root, p takes it as with fresh roots. Elsewhere, a root
	// listed before may have been renamed or moved above p since it was read.
	if !under(l.roots[:1], p) {
		if !l.read {
			l.read = true
			var scope sharing.FilePathScope
			if scope, l.err = sharing.ResolveFilePaths(l.inst, l.perm); l.err == nil {
				l.roots = scope.Roots
			}
		}
		if l.err != nil || !under(l.roots, p) {
			return ""
		}
	}
	return vfs.RelativeToLink(l.roots, p)
}

// under tells if p is one of the roots or under one of them.
func under(roots []vfs.LinkRoot, p string) bool {
	for _, root := range roots {
		if p == root.Path || strings.HasPrefix(p, root.Path+"/") {
			return true
		}
	}
	return false
}
