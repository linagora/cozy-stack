package realtime

import (
	"path"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// linkPaths rewrites the paths of the documents sent to the holder of a public
// link, to start at the roots of the link. It is used by the goroutine that
// writes the events only.
type linkPaths struct {
	inst  *instance.Instance
	perm  *permission.Permission
	roots []vfs.LinkRoot
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
	return vfs.RewriteEventPaths(doc, l.relative)
}

func (l *linkPaths) relative(id, _, p string) string {
	if p == "" {
		return ""
	}
	if id != "" {
		for _, root := range l.roots {
			if root.ID == id {
				if p == "/" {
					return p
				}
				return "/" + path.Base(p)
			}
		}
	}
	if rel := vfs.RelativeToLink(l.roots, p); rel != "" {
		return rel
	}
	// A folder above a root may have been renamed or moved.
	if scope, err := sharing.ResolveFilePaths(l.inst, l.perm); err == nil {
		l.roots = scope.Roots
		return vfs.RelativeToLink(l.roots, p)
	}
	return ""
}
