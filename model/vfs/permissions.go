package vfs

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
)

// Fetcher extends on permission.Fetcher with hierarchy functions
type Fetcher interface {
	permission.Fetcher
	parentID() string
	Path(fs FilePather) (string, error)
	Parent(fs VFS) (*DirDoc, error)
}

// FileDoc & DirDoc are vfs.Fetcher
var _ Fetcher = (*FileDoc)(nil)
var _ Fetcher = (*DirDoc)(nil)

// Allows check if a permSet allows verb on given file
func Allows(fs VFS, pset permission.Set, v permission.Verb, fd Fetcher) error {
	allowedIDs := []string{}
	otherRules := []permission.Rule{}

	// First pass, we iterate over the rules, check if we have an easy match
	// keep a short list of useful rules and allowed IDs.
	for _, r := range pset {
		typ := permission.TrimWildcard(r.Type)
		if typ != consts.Files && !strings.HasPrefix(consts.Files, typ+".") {
			continue
		}
		if !r.Verbs.Contains(v) {
			continue
		}

		// permission on whole io.cozy.files doctype
		if len(r.Values) == 0 {
			return nil
		}

		// permission by ID directly on self, parent or root
		if r.Selector == "" {
			for _, v := range r.Values {
				if v == fd.ID() || v == fd.parentID() || v == consts.RootDirID {
					return nil
				}
				allowedIDs = append(allowedIDs, v)
			}
		}

		// permission by attributes values (tags, mime ...) on self
		valid := func(value string) bool {
			candidates := fd.Fetch(r.Selector)
			for _, candidate := range candidates {
				if value == candidate {
					return true
				}
			}
			return false
		}
		if r.SomeValue(valid) {
			return nil
		}

		// store rules that could apply to an ancestor
		if r.Selector != "mime" && r.Selector != "class" {
			otherRules = append(otherRules, r)
		}
	}

	// We have some rules on IDs, let's fetch their paths and check if they are
	// ancestors of current object
	if len(allowedIDs) > 0 {
		selfPath, err := fd.Path(fs)
		if err != nil {
			return err
		}

		for _, id := range allowedIDs {
			allowedPath, err := pathFromID(fs, id)
			// tested is children of allowed ( err is ignored, it most probably
			// means a permissions on a deleted directory)
			if err == nil && strings.HasPrefix(selfPath, allowedPath+"/") {
				return nil
			}
		}
	}

	// We have some rules on attributes, let's iterate over the current object
	// ancestors and check if any match the rules
	if len(otherRules) > 0 && fd.ID() != consts.RootDirID {
		cur, err := fd.Parent(fs)
		if err != nil {
			return err
		}
		for cur.ID() != consts.RootDirID {
			for _, rule := range otherRules {
				if rule.ValuesMatch(cur) {
					return nil
				}
			}
			cur, err = cur.Parent(fs)
			if err != nil {
				return err
			}
		}
	}

	// no match : game over !
	return errNoPermission
}

var errNoPermission = errors.New("no permission")

// AllowsRule checks that the documents targeted by id in the rule r are
// accessible with every verb of r through the parent set. It is a semantic
// fallback for Set.RuleInSubset, which cannot tell for example that a file
// is inside a directory matched by a referenced_by selector. It returns
// permission.ErrNotSubset if the rule is not allowed, or doesn't target
// io.cozy.files by id.
func AllowsRule(fs VFS, parent permission.Set, r permission.Rule) error {
	// Exact type only: a wildcard type would also match other doctypes by id.
	if r.Type != consts.Files || r.Selector != "" || len(r.Values) == 0 {
		return permission.ErrNotSubset
	}
	for _, id := range r.Values {
		dir, file, err := fs.DirOrFileByID(id)
		if os.IsNotExist(err) {
			return permission.ErrNotSubset
		}
		if err != nil {
			return err
		}
		var fd Fetcher = file
		if dir != nil {
			fd = dir
		}
		for v := range r.Verbs {
			err := Allows(fs, parent, v, fd)
			if errors.Is(err, errNoPermission) {
				return permission.ErrNotSubset
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func pathFromID(fs VFS, id string) (string, error) {
	if id == consts.RootDirID {
		return "", nil
	}

	if id == consts.TrashDirID {
		return TrashDirName, nil
	}

	dir, err := fs.DirByID(id)
	if err != nil {
		return "", err
	}

	return dir.Path(fs)
}

func (f *FileDoc) parentID() string { return f.DirID }
func (d *DirDoc) parentID() string  { return d.DirID }

// Fetch implements permission.Fetch on FileDoc
func (f *FileDoc) Fetch(field string) []string {
	switch field {
	case "type":
		return []string{f.Type}
	case "name":
		return []string{f.DocName}
	case "mime":
		return []string{f.Mime}
	case "class":
		return []string{f.Class}
	case "tags":
		return f.Tags
	case "referenced_by":
		if f != nil {
			var values []string
			for _, ref := range f.ReferencedBy {
				// 2 formats are possible:
				// - only the identifier
				// - doctype/docid
				values = append(values, ref.ID, fmt.Sprintf("%s/%s", ref.Type, ref.ID))
			}
			return values
		}
	}
	return nil
}

// Fetch implements permission.Fetcher on DirDOc
func (d *DirDoc) Fetch(field string) []string {
	switch field {
	case "type":
		return []string{d.Type}
	case "name":
		return []string{d.DocName}
	case "tags":
		return d.Tags
	case "referenced_by":
		var values []string
		for _, ref := range d.ReferencedBy {
			values = append(values, ref.ID)
		}
		return values
	}
	return nil
}
