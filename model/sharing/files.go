package sharing

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/note"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/metadata"
	multierror "github.com/hashicorp/go-multierror"
)

// isTrashed returns true for a file or folder inside the trash
func isTrashed(doc couchdb.JSONDoc) bool {
	if doc.Type != consts.Files {
		return false
	}
	if doc.Get("type") == consts.FileType {
		return doc.Get("trashed") == true
	}
	return strings.HasPrefix(doc.Get("path").(string), vfs.TrashDirName+"/")
}

// MakeXorKey generates a key for transforming the file identifiers
func MakeXorKey() []byte {
	random := crypto.GenerateRandomBytes(8)
	result := make([]byte, 2*len(random))
	for i, val := range random {
		result[2*i] = val & 0xf
		result[2*i+1] = val >> 4
	}
	return result
}

// XorID transforms the identifier of a file to a new identifier, in a
// reversible way: it makes a XOR on the hexadecimal characters
func XorID(id string, key []byte) string {
	l := len(key)
	buf := []byte(id)
	for i, c := range buf {
		switch {
		case '0' <= c && c <= '9':
			c = (c - '0') ^ key[i%l]
		case 'a' <= c && c <= 'f':
			c = (c - 'a' + 10) ^ key[i%l]
		case 'A' <= c && c <= 'F':
			c = (c - 'A' + 10) ^ key[i%l]
		default:
			continue
		}
		if c < 10 {
			buf[i] = c + '0'
		} else {
			buf[i] = (c - 10) + 'a'
		}
	}
	return string(buf)
}

// SortFilesToSent sorts the files slice that will be sent in bulk_docs:
//   - directories must come before files (if a file is created in a new
//     directory, we must create directory before the file)
//   - directories are sorted by increasing depth (if a sub-folder is created
//     in a new directory, we must create the parent before the child)
//   - deleted elements must come at the end, to efficiently cope with moves.
//     For example, if we have A->B->C hierarchy and C is moved elsewhere
//     and B deleted, we must make the move before deleting B and its children.
func (s *Sharing) SortFilesToSent(files []map[string]interface{}) {
	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if removed, ok := a["_deleted"].(bool); ok && removed {
			return false
		}
		if removed, ok := b["_deleted"].(bool); ok && removed {
			return true
		}
		if a["type"] == consts.FileType {
			return false
		}
		if b["type"] == consts.FileType {
			return true
		}
		q, ok := b["path"].(string)
		if !ok {
			return false
		}
		p, ok := a["path"].(string)
		if !ok {
			return true
		}
		return strings.Count(p, "/") < strings.Count(q, "/")
	})
}

// TransformFileToSent transforms an io.cozy.files document before sending it
// to another cozy instance:
// - its identifier is XORed
// - its dir_id is XORed or removed
// - the referenced_by are XORed or removed
// - the path is removed (directory only)
//
// ruleIndexes is a map of "doctype-docid" -> rule index
func (s *Sharing) TransformFileToSent(doc map[string]interface{}, xorKey []byte, ruleIndex int) {
	if doc["type"] == consts.DirType {
		delete(doc, "path")
		delete(doc, "not_synchronized_on")
	}
	id := doc["_id"].(string)
	doc["_id"] = XorID(id, xorKey)
	dir, ok := doc["dir_id"].(string)
	if !ok {
		return
	}
	rule := s.Rules[ruleIndex]
	var noDirID bool
	if rule.Selector == couchdb.SelectorReferencedBy {
		noDirID = true
		if refs, ok := doc[couchdb.SelectorReferencedBy].([]interface{}); ok {
			kept := make([]interface{}, 0)
			for _, ref := range refs {
				if r, ok := ref.(map[string]interface{}); ok {
					v := r["type"].(string) + "/" + r["id"].(string)
					for _, val := range rule.Values {
						if val == v {
							r["id"] = XorID(r["id"].(string), xorKey)
							kept = append(kept, r)
							break
						}
					}
				}
			}
			doc[couchdb.SelectorReferencedBy] = kept
		}
	} else {
		for _, v := range rule.Values {
			if v == id {
				noDirID = true
			}
		}
		delete(doc, couchdb.SelectorReferencedBy)
	}
	if !noDirID {
		for _, v := range rule.Values {
			if v == dir {
				noDirID = true
				break
			}
		}
	}
	if noDirID {
		delete(doc, "dir_id")
	} else {
		doc["dir_id"] = XorID(dir, xorKey)
	}
}

// EnsureSharedWithMeDir returns the shared-with-me directory, and create it if
// it doesn't exist
func EnsureSharedWithMeDir(inst *instance.Instance) (*vfs.DirDoc, error) {
	fs := inst.VFS()
	dir, _, err := fs.DirOrFileByID(consts.SharedWithMeDirID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		inst.Logger().WithNamespace("sharing").
			Warnf("EnsureSharedWithMeDir failed to find the dir: %s", err)
		return nil, err
	}

	if dir == nil {
		name := inst.Translate("Tree Shared with me")
		dir, err = vfs.NewDirDocWithPath(name, consts.RootDirID, "/", nil)
		if err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("EnsureSharedWithMeDir failed to make the dir: %s", err)
			return nil, err
		}
		dir.DocID = consts.SharedWithMeDirID
		dir.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
		err = fs.CreateDir(dir)
		if errors.Is(err, os.ErrExist) {
			dir, err = fs.DirByPath(dir.Fullpath)
		}
		if err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("EnsureSharedWithMeDir failed to create the dir: %s", err)
			return nil, err
		}
		return dir, nil
	}

	if dir.RestorePath != "" {
		now := time.Now()
		instanceURL := inst.PageURL("/", nil)
		if dir.CozyMetadata == nil {
			dir.CozyMetadata = vfs.NewCozyMetadata(instanceURL)
		} else {
			dir.CozyMetadata.UpdatedAt = now
		}
		dir, err = vfs.RestoreDir(fs, dir)
		if err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("EnsureSharedWithMeDir failed to restore the dir: %s", err)
			return nil, err
		}
		children, err := fs.DirBatch(dir, couchdb.NewSkipCursor(0, 0))
		if err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("EnsureSharedWithMeDir failed to find children: %s", err)
			return nil, err
		}
		for _, child := range children {
			d, f := child.Refine()
			if d != nil {
				if d.CozyMetadata == nil {
					d.CozyMetadata = vfs.NewCozyMetadata(instanceURL)
				} else {
					d.CozyMetadata.UpdatedAt = now
				}
				_, err = vfs.TrashDir(fs, d)
			} else {
				if f.CozyMetadata == nil {
					f.CozyMetadata = vfs.NewCozyMetadata(instanceURL)
				} else {
					f.CozyMetadata.UpdatedAt = now
				}
				_, err = vfs.TrashFile(fs, f)
			}
			if err != nil {
				inst.Logger().WithNamespace("sharing").
					Warnf("EnsureSharedWithMeDir failed to trash children: %s", err)
				return nil, err
			}
		}
	}

	return dir, nil
}

// CreateDirForSharing creates the directory where files for this sharing will
// be put. This directory will be initially inside the Shared with me folder.
func (s *Sharing) CreateDirForSharing(inst *instance.Instance, rule *Rule, parentID string) (*vfs.DirDoc, error) {
	fs := inst.VFS()
	var err error
	var parent *vfs.DirDoc
	if parentID == "" {
		parent, err = EnsureSharedWithMeDir(inst)
	} else {
		parent, err = fs.DirByID(parentID)
	}
	if err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("CreateDirForSharing failed to find parent directory: %s", err)
		return nil, err
	}
	dir, err := vfs.NewDirDocWithParent(rule.Title, parent, []string{"from-sharing-" + s.SID})
	if err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("CreateDirForSharing failed to make dir: %s", err)
		return nil, err
	}
	parts := strings.Split(rule.Values[0], "/")
	dir.DocID = parts[len(parts)-1]
	dir.AddReferencedBy(s.DocReference())
	dir.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
	basename := dir.DocName
	for i := 2; i < 20; i++ {
		if err = fs.CreateDir(dir); err == nil {
			return dir, nil
		}
		if couchdb.IsConflictError(err) || errors.Is(err, os.ErrExist) {
			doc, err := fs.DirByID(dir.DocID)
			if err == nil {
				doc.AddReferencedBy(s.DocReference())
				_ = couchdb.UpdateDoc(inst, doc)
				return doc, nil
			}
		}
		dir.DocName = fmt.Sprintf("%s (%d)", basename, i)
		dir.Fullpath = path.Join(parent.Fullpath, dir.DocName)
	}
	inst.Logger().WithNamespace("sharing").
		Errorf("Cannot create the sharing directory: %s", err)
	return nil, err
}

// AddReferenceForSharing adds a reference to the sharing on the sharing
// root. For drive sharings, the root can be either a directory or a file.
func (s *Sharing) AddReferenceForSharing(inst *instance.Instance, rule *Rule) error {
	fs := inst.VFS()
	parts := strings.Split(rule.Values[0], "/")
	dir, file, err := fs.DirOrFileByID(parts[len(parts)-1])
	if err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("AddReferenceForSharing failed to find root: %s", err)
		return err
	}
	if dir == nil && file == nil {
		return nil
	}

	if dir != nil {
		for _, ref := range dir.ReferencedBy {
			if ref == s.DocReference() {
				return nil
			}
		}
		olddoc := dir.Clone().(*vfs.DirDoc)
		dir.AddReferencedBy(s.DocReference())
		if dir.CozyMetadata == nil {
			dir.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
		} else {
			dir.CozyMetadata.UpdatedAt = time.Now()
		}
		return fs.UpdateDirDoc(olddoc, dir)
	}

	if !s.Drive {
		return nil
	}
	for _, ref := range file.ReferencedBy {
		if ref == s.DocReference() {
			return nil
		}
	}
	olddoc := file.Clone().(*vfs.FileDoc)
	file.AddReferencedBy(s.DocReference())
	if file.CozyMetadata == nil {
		file.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
	} else {
		file.CozyMetadata.UpdatedAt = time.Now()
	}
	return fs.UpdateFileDoc(olddoc, file)
}

// RemoveReferenceForSharing removes the reference to the sharing from its root
// file or folder. It is a no-op when the root is already gone.
func (s *Sharing) RemoveReferenceForSharing(inst *instance.Instance, rule *Rule) error {
	if rule == nil || rule.Selector == couchdb.SelectorReferencedBy || len(rule.Values) == 0 {
		return nil
	}

	indexer := vfs.NewCouchdbIndexer(inst)
	parts := strings.Split(rule.Values[0], "/")
	dir, file, err := indexer.DirOrFileByID(parts[len(parts)-1])
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || couchdb.IsNotFoundError(err) {
			return nil
		}
		inst.Logger().WithNamespace("sharing").
			Warnf("RemoveReferenceForSharing failed to find root: %s", err)
		return err
	}
	if dir == nil && file == nil {
		return nil
	}

	ref := s.DocReference()

	if dir != nil {
		newdoc := dir.Clone().(*vfs.DirDoc)
		before := len(newdoc.ReferencedBy)
		newdoc.RemoveReferencedBy(ref)
		if len(newdoc.ReferencedBy) == before {
			return nil
		}
		if newdoc.CozyMetadata != nil {
			newdoc.CozyMetadata.UpdatedAt = time.Now()
		}
		return indexer.UpdateDirDoc(dir, newdoc)
	}

	newdoc := file.Clone().(*vfs.FileDoc)
	before := len(newdoc.ReferencedBy)
	newdoc.RemoveReferencedBy(ref)
	if len(newdoc.ReferencedBy) == before {
		return nil
	}
	if newdoc.CozyMetadata != nil {
		newdoc.CozyMetadata.UpdatedAt = time.Now()
	}
	return indexer.UpdateFileDoc(file, newdoc)
}

// ValidateDriveRoot validates that a file or folder can be used to create a
// shared drive.
func ValidateDriveRoot(inst *instance.Instance, rootID string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	if rootID == consts.RootDirID ||
		rootID == consts.TrashDirID ||
		rootID == consts.SharedWithMeDirID ||
		rootID == consts.NoLongerSharedDirID ||
		rootID == consts.SharedDrivesDirID {
		return nil, nil, ErrSystemFolder
	}

	fs := inst.VFS()
	dir, file, err := fs.DirOrFileByID(rootID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrDriveRootNotFound
		}
		return nil, nil, err
	}
	if dir == nil && file == nil {
		return nil, nil, ErrDriveRootNotFound
	}
	if dir != nil {
		if strings.HasPrefix(dir.Fullpath, vfs.TrashDirName+"/") {
			return nil, nil, ErrSystemFolder
		}
		if err := checkRootForSharing(dir.ReferencedBy, ErrFolderAlreadyShared); err != nil {
			return nil, nil, err
		}
		if err := checkParentsForSharing(fs, path.Dir(dir.Fullpath), ErrFolderAlreadyShared); err != nil {
			return nil, nil, err
		}
		if err := checkDescendantsForSharing(fs, dir); err != nil {
			return nil, nil, err
		}
		return dir, nil, nil
	}
	if file.Trashed {
		return nil, nil, ErrFileInTrash
	}
	if err := checkRootForSharing(file.ReferencedBy, ErrFileAlreadyShared); err != nil {
		return nil, nil, err
	}
	filePath, err := file.Path(fs)
	if err != nil {
		return nil, nil, err
	}
	if err := checkParentsForSharing(fs, path.Dir(filePath), ErrFileAlreadyShared); err != nil {
		return nil, nil, err
	}
	return nil, file, nil
}

func checkRootForSharing(refs []couchdb.DocReference, alreadySharedErr error) error {
	for _, ref := range refs {
		if ref.Type == consts.Sharings {
			return alreadySharedErr
		}
	}
	return nil
}

func checkParentsForSharing(fs vfs.VFS, currentPath string, alreadySharedErr error) error {
	for currentPath != "/" && currentPath != "." && currentPath != "" {
		parentDir, err := fs.DirByPath(currentPath)
		if err != nil {
			break
		}
		if err := checkRootForSharing(parentDir.ReferencedBy, alreadySharedErr); err != nil {
			return err
		}
		currentPath = path.Dir(currentPath)
	}
	return nil
}

// checkDescendantsForSharing recursively checks if any descendant directory
// has an existing sharing reference.
func checkDescendantsForSharing(fs vfs.VFS, dir *vfs.DirDoc) error {
	iter := fs.DirIterator(dir, nil)
	for {
		child, _, err := iter.Next()
		if err == vfs.ErrIteratorDone {
			break
		}
		if err != nil {
			return err
		}
		if child == nil {
			continue
		}

		// Check if this child directory has a sharing reference
		for _, ref := range child.ReferencedBy {
			if ref.Type == consts.Sharings {
				return ErrFolderAlreadyShared
			}
		}

		// Recursively check subdirectories
		if err := checkDescendantsForSharing(fs, child); err != nil {
			return err
		}
	}
	return nil
}

// GetSharingDir returns the directory used by this sharing for putting files
// and folders that have no dir_id.
func (s *Sharing) GetSharingDir(inst *instance.Instance) (*vfs.DirDoc, error) {
	// When we can, find the sharing dir by its ID
	fs := inst.VFS()
	rule := s.FirstFilesRule()
	if rule != nil {
		if rule.Mime != "" {
			inst.Logger().WithNamespace("sharing").
				Warnf("GetSharingDir called for only one file: %s", s.SID)
			return nil, ErrInternalServerError
		}
		rootID, err := s.DriveRootID()
		if err == nil {
			dir, _ := fs.DirByID(rootID)
			if dir != nil {
				return dir, nil
			}
		}
	}

	// Else, try to find it by a reference
	key := []string{consts.Sharings, s.SID}
	end := []string{key[0], key[1], couchdb.MaxString}
	req := &couchdb.ViewRequest{
		StartKey:    key,
		EndKey:      end,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(inst, couchdb.FilesReferencedByView, req, &res)
	if err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("Sharing dir not found: %v (%s)", err, s.SID)
		return nil, ErrInternalServerError
	}
	var parentID string
	if len(res.Rows) > 0 {
		dir, file, err := fs.DirOrFileByID(res.Rows[0].ID)
		if err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("GetSharingDir failed to find dir: %s", err)
			return dir, err
		}
		if dir != nil {
			return dir, nil
		}
		// file is a shortcut
		parentID = file.DirID
		if err := fs.DestroyFile(file); err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("GetSharingDir failed to delete shortcut: %s", err)
			return nil, err
		}
		s.ShortcutID = ""
		_ = couchdb.UpdateDoc(inst, s)
	}
	if rule == nil {
		inst.Logger().WithNamespace("sharing").
			Errorf("no first rule for: %#v", s)
		return nil, ErrInternalServerError
	}
	// And, we may have to create it in last resort
	return s.CreateDirForSharing(inst, rule, parentID)
}

// RemoveSharingDir removes the reference on the sharing directory, and adds a
// suffix to its name: the suffix will help make the user understand that the
// sharing has been revoked, and it will avoid conflicts if the user accepts a
// new sharing for the same folder. It should be called when a sharing is
// revoked, on the recipient Cozy.
func (s *Sharing) RemoveSharingDir(inst *instance.Instance) error {
	dir, err := s.GetSharingDir(inst)
	if couchdb.IsNotFoundError(err) {
		return nil
	} else if err != nil {
		return err
	}
	olddoc := dir.Clone().(*vfs.DirDoc)
	dir.RemoveReferencedBy(s.DocReference())
	if dir.CozyMetadata == nil {
		dir.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
	} else {
		dir.CozyMetadata.UpdatedAt = time.Now()
	}
	suffix := inst.Translate("Tree Revoked sharing suffix")
	parentPath := filepath.Dir(dir.Fullpath)
	basename := fmt.Sprintf("%s (%s)", dir.DocName, suffix)
	dir.DocName = basename
	for i := 2; i < 100; i++ {
		dir.Fullpath = path.Join(parentPath, dir.DocName)
		if err = inst.VFS().UpdateDirDoc(olddoc, dir); err == nil {
			return nil
		}
		dir.DocName = fmt.Sprintf("%s (%d)", basename, i)
	}
	return err
}

// GetNoLongerSharedDir returns the directory used for files and folders that
// are removed from a sharing, but are still used via a reference. It the
// directory does not exist, it is created.
func (s *Sharing) GetNoLongerSharedDir(inst *instance.Instance) (*vfs.DirDoc, error) {
	fs := inst.VFS()
	dir, _, err := fs.DirOrFileByID(consts.NoLongerSharedDirID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if dir == nil {
		parent, errp := EnsureSharedWithMeDir(inst)
		if errp != nil {
			return nil, errp
		}
		if strings.HasPrefix(parent.Fullpath, vfs.TrashDirName) {
			parent, errp = fs.DirByID(consts.RootDirID)
			if errp != nil {
				return nil, errp
			}
		}
		name := inst.Translate("Tree No longer shared")
		dir, err = vfs.NewDirDocWithParent(name, parent, nil)
		if err != nil {
			return nil, err
		}
		dir.DocID = consts.NoLongerSharedDirID
		dir.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
		if err = fs.CreateDir(dir); err != nil {
			return nil, err
		}
		return dir, nil
	}

	if dir.RestorePath != "" {
		now := time.Now()
		instanceURL := inst.PageURL("/", nil)
		if dir.CozyMetadata == nil {
			dir.CozyMetadata = vfs.NewCozyMetadata(instanceURL)
		} else {
			dir.CozyMetadata.UpdatedAt = now
		}
		dir.CozyMetadata.UpdatedAt = now
		_, err = vfs.RestoreDir(fs, dir)
		if err != nil {
			return nil, err
		}
		children, err := fs.DirBatch(dir, couchdb.NewSkipCursor(0, 0))
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			d, f := child.Refine()
			if d != nil {
				if d.CozyMetadata == nil {
					d.CozyMetadata = vfs.NewCozyMetadata(instanceURL)
				} else {
					d.CozyMetadata.UpdatedAt = now
				}
				_, err = vfs.TrashDir(fs, d)
			} else {
				if f.CozyMetadata == nil {
					f.CozyMetadata = vfs.NewCozyMetadata(instanceURL)
				} else {
					f.CozyMetadata.UpdatedAt = now
				}
				_, err = vfs.TrashFile(fs, f)
			}
			if err != nil {
				return nil, err
			}
		}
	}

	return dir, nil
}

// GetFolder returns informations about a folder (with XORed IDs)
func (s *Sharing) GetFolder(inst *instance.Instance, m *Member, xoredID string) (map[string]interface{}, error) {
	creds := s.FindCredentials(m)
	if creds == nil {
		return nil, ErrInvalidSharing
	}
	dirID := XorID(xoredID, creds.XorKey)
	ref := &SharedRef{}
	err := couchdb.GetDoc(inst, consts.Shared, consts.Files+"/"+dirID, ref)
	if err != nil {
		return nil, err
	}
	info, ok := ref.Infos[s.SID]
	if !ok || info.Removed {
		return nil, ErrFolderNotFound
	}
	dir, err := inst.VFS().DirByID(dirID)
	if err != nil {
		return nil, err
	}
	doc := dirToJSONDoc(dir, inst.PageURL("/", nil)).M
	s.TransformFileToSent(doc, creds.XorKey, info.Rule)
	return doc, nil
}

// ApplyBulkFiles takes a list of documents for the io.cozy.files doctype and
// will apply changes to the VFS according to those documents.
func (s *Sharing) ApplyBulkFiles(inst *instance.Instance, docs DocsList) error {
	type retryOp struct {
		target map[string]interface{}
		dir    *vfs.DirDoc
		ref    *SharedRef
	}

	var errm error
	var retries []retryOp
	fs := inst.VFS()

	for _, target := range docs {
		id, ok := target["_id"].(string)
		if !ok {
			errm = multierror.Append(errm, ErrMissingID)
			continue
		}
		ref := &SharedRef{}
		err := couchdb.GetDoc(inst, consts.Shared, consts.Files+"/"+id, ref)
		if err != nil {
			if !couchdb.IsNotFoundError(err) {
				inst.Logger().WithNamespace("replicator").
					Debugf("Error on finding doc of bulk files: %s", err)
				errm = multierror.Append(errm, err)
				continue
			}
			ref = nil
		}
		var infos SharedInfo
		if ref != nil {
			infos, ok = ref.Infos[s.SID]
			if !ok {
				inst.Logger().WithNamespace("replicator").
					Infof("Operation aborted for %s on sharing %s", id, s.SID)
				errm = multierror.Append(errm, ErrSafety)
				continue
			}
		}
		dir, file, err := fs.DirOrFileByID(id)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			inst.Logger().WithNamespace("replicator").
				Debugf("Error on finding ref of bulk files: %s", err)
			errm = multierror.Append(errm, err)
			continue
		}
		if _, ok := target["_deleted"]; ok {
			if ref == nil || infos.Removed {
				continue
			}
			if dir == nil && file == nil {
				continue
			}
			if dir != nil {
				err = s.TrashDir(inst, dir)
			} else {
				err = s.TrashFile(inst, file, &s.Rules[infos.Rule])
			}
		} else if target["type"] != consts.DirType {
			// Let the upload worker manages this file
			continue
		} else if ref != nil && infos.Removed && !infos.Dissociated {
			continue
		} else if dir == nil {
			err = s.CreateDir(inst, target, delayResolution)
			if errors.Is(err, os.ErrExist) {
				retries = append(retries, retryOp{
					target: target,
				})
				err = nil
			}
		} else if ref == nil || infos.Dissociated {
			// If it is a file: let the upload worker manages this file
			// If it is a dir: ignore this (safety rule)
			continue
		} else {
			// XXX we have to clone the dir document as it is modified by the
			// UpdateDir function and retrying the operation won't work with
			// the modified doc
			cloned := dir.Clone().(*vfs.DirDoc)
			err = s.UpdateDir(inst, target, dir, ref, delayResolution)
			if errors.Is(err, os.ErrExist) {
				retries = append(retries, retryOp{
					target: target,
					dir:    cloned,
					ref:    ref,
				})
				err = nil
			}
		}
		if err != nil {
			inst.Logger().WithNamespace("replicator").
				Debugf("Error on apply bulk file: %s (%#v - %#v)", err, target, ref)
			errm = multierror.Append(errm, fmt.Errorf("%s - %w", id, err))
		}
	}

	for _, op := range retries {
		var err error
		if op.dir == nil {
			err = s.CreateDir(inst, op.target, resolveResolution)
		} else {
			err = s.UpdateDir(inst, op.target, op.dir, op.ref, resolveResolution)
		}
		if err != nil {
			inst.Logger().WithNamespace("replicator").
				Debugf("Error on apply bulk file: %s (%#v - %#v)", err, op.target, op.ref)
			errm = multierror.Append(errm, err)
		}
	}

	return errm
}

func (s *Sharing) GetNotes(inst *instance.Instance) ([]*vfs.FileDoc, error) {
	rule := s.FirstFilesRule()
	if rule != nil {
		if rule.Mime != "" {
			if rule.Mime == consts.NoteMimeType {
				var notes []*vfs.FileDoc
				req := &couchdb.AllDocsRequest{Keys: rule.Values}
				if err := couchdb.GetAllDocs(inst, consts.Files, req, &notes); err != nil {
					return nil, fmt.Errorf("failed to fetch notes shared by themselves: %w", err)
				}

				return nonNilFileDocs(notes), nil
			} else {
				return nil, nil
			}
		}

		sharingDir, err := s.GetSharingDir(inst)
		if err != nil {
			return nil, fmt.Errorf("failed to get notes sharing dir: %w", err)
		}

		var notes []*vfs.FileDoc
		fs := inst.VFS()
		iter := fs.DirIterator(sharingDir, nil)
		for {
			_, f, err := iter.Next()
			if errors.Is(err, vfs.ErrIteratorDone) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to get next shared note: %w", err)
			}
			if f != nil && f.Mime == consts.NoteMimeType {
				notes = append(notes, f)
			}
		}

		return notes, nil
	}

	return nil, nil
}

func nonNilFileDocs(files []*vfs.FileDoc) []*vfs.FileDoc {
	docs := files[:0]
	for _, file := range files {
		if file != nil {
			docs = append(docs, file)
		}
	}
	return docs
}

func (s *Sharing) FixRevokedNotes(inst *instance.Instance) error {
	docs, err := s.GetNotes(inst)
	if err != nil {
		return fmt.Errorf("failed to get revoked sharing notes: %w", err)
	}

	var errm error
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		// If the note came from another cozy via a sharing that is now revoked, we
		// may need to recreate the trigger.
		if err := note.SetupTrigger(inst, doc.ID()); err != nil {
			errm = multierror.Append(errm, fmt.Errorf("failed to setup revoked note trigger: %w", err))
		}

		if err := note.ImportImages(inst, doc); err != nil {
			errm = multierror.Append(errm, fmt.Errorf("failed to import revoked note images: %w", err))
		}
	}
	return errm
}

func removeReferencesFromRule(file *vfs.FileDoc, rule *Rule) {
	if rule.Selector != couchdb.SelectorReferencedBy {
		return
	}
	refs := file.ReferencedBy[:0]
	for _, ref := range file.ReferencedBy {
		if !rule.hasReferencedBy(ref) {
			refs = append(refs, ref)
		}
	}
	file.ReferencedBy = refs
}

func buildReferencedBy(target, file *vfs.FileDoc, rule *Rule) []couchdb.DocReference {
	refs := make([]couchdb.DocReference, 0)
	if file != nil {
		for _, ref := range file.ReferencedBy {
			if !rule.hasReferencedBy(ref) {
				refs = append(refs, ref)
			}
		}
	}
	for _, ref := range target.ReferencedBy {
		if rule.hasReferencedBy(ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

func copySafeFieldsToFile(target, file *vfs.FileDoc) {
	file.Tags = target.Tags
	file.Metadata = target.Metadata.RemoveCertifiedMetadata()
	file.CreatedAt = target.CreatedAt
	file.UpdatedAt = target.UpdatedAt
	file.Mime = target.Mime
	file.Class = target.Class
	file.Executable = target.Executable
	file.CozyMetadata = target.CozyMetadata
}

func copySafeFieldsToDir(target map[string]interface{}, dir *vfs.DirDoc) {
	if tags, ok := target["tags"].([]interface{}); ok {
		dir.Tags = make([]string, 0, len(tags))
		for _, tag := range tags {
			if t, ok := tag.(string); ok {
				dir.Tags = append(dir.Tags, t)
			}
		}
	}
	if created, ok := target["created_at"].(string); ok {
		if at, err := time.Parse(time.RFC3339Nano, created); err == nil {
			dir.CreatedAt = at
		}
	}
	if updated, ok := target["updated_at"].(string); ok {
		if at, err := time.Parse(time.RFC3339Nano, updated); err == nil {
			dir.UpdatedAt = at
		}
	}

	if meta, ok := target["metadata"].(map[string]interface{}); ok {
		dir.Metadata = vfs.Metadata(meta).RemoveCertifiedMetadata()
	}

	if meta, ok := target["cozyMetadata"].(map[string]interface{}); ok {
		dir.CozyMetadata = &vfs.FilesCozyMetadata{}
		if version, ok := meta["doctypeVersion"].(string); ok {
			dir.CozyMetadata.DocTypeVersion = version
		}
		if version, ok := meta["metadataVersion"].(float64); ok {
			dir.CozyMetadata.MetadataVersion = int(version)
		}
		if created, ok := meta["createdAt"].(string); ok {
			if at, err := time.Parse(time.RFC3339Nano, created); err == nil {
				dir.CozyMetadata.CreatedAt = at
			}
		}
		if app, ok := meta["createdByApp"].(string); ok {
			dir.CozyMetadata.CreatedByApp = app
		}
		if version, ok := meta["createdByAppVersion"].(string); ok {
			dir.CozyMetadata.CreatedByAppVersion = version
		}
		if instance, ok := meta["createdOn"].(string); ok {
			dir.CozyMetadata.CreatedOn = instance
		}

		if updated, ok := meta["updatedAt"].(string); ok {
			if at, err := time.Parse(time.RFC3339Nano, updated); err == nil {
				dir.CozyMetadata.UpdatedAt = at
			}
		}
		if trashed, ok := meta["trashedAt"].(string); ok {
			if at, err := time.Parse(time.RFC3339Nano, trashed); err == nil {
				dir.CozyMetadata.TrashedAt = &at
			}
		}
		if updates, ok := meta["updatedByApps"].([]map[string]interface{}); ok {
			for _, update := range updates {
				if slug, ok := update["slug"].(string); ok {
					entry := &metadata.UpdatedByAppEntry{Slug: slug}
					if date, ok := update["date"].(string); ok {
						if at, err := time.Parse(time.RFC3339Nano, date); err == nil {
							entry.Date = at
						}
					}
					if version, ok := update["version"].(string); ok {
						entry.Version = version
					}
					if instance, ok := update["instance"].(string); ok {
						entry.Instance = instance
					}
					dir.CozyMetadata.UpdatedByApps = append(dir.CozyMetadata.UpdatedByApps, entry)
				}
			}
		}
		if trashedBy, ok := meta["trashedBy"].(map[string]interface{}); ok {
			entry := &vfs.TrashedByEntry{}
			if kind, ok := trashedBy["kind"].(string); ok {
				entry.Kind = kind
			}
			if displayName, ok := trashedBy["displayName"].(string); ok {
				entry.DisplayName = displayName
			}
			if domain, ok := trashedBy["domain"].(string); ok {
				entry.Domain = domain
			} else if legacyInstance, ok := trashedBy["instance"].(string); ok {
				entry.Domain = legacyInstance
			}
			dir.CozyMetadata.TrashedBy = entry
		}

		// No upload* for directories
		if account, ok := meta["sourceAccount"].(string); ok {
			dir.CozyMetadata.SourceAccount = account
		}
		if id, ok := meta["sourceAccountIdentifier"].(string); ok {
			dir.CozyMetadata.SourceIdentifier = id
		}
	}
}

// resolveConflictSamePath is used when two files/folders are in conflict
// because they have the same path. To resolve the conflict, we take the
// file/folder from the owner instance as the winner and rename the other.
//
// Note: previously, the rule was that the higher id wins, but the rule has
// been changed. The new rule helps to minimize the number of exchanges needed
// between the cozy instance to converge, and, as such, it helps to avoid
// creating more conflicts.
//
// If the winner is the new file/folder from the other cozy, this function
// rename the local file/folder and let the caller retry its operation.
// If the winner is the local file/folder, this function returns the new name
// and let the caller do its operation with the new name (the caller should
// create a dummy revision to let the other cozy know of the renaming).
func (s *Sharing) resolveConflictSamePath(inst *instance.Instance, visitorID, pth string) (string, error) {
	inst.Logger().WithNamespace("replicator").
		Infof("Resolve conflict for path=%s (docid=%s)", pth, visitorID)
	fs := inst.VFS()
	d, f, err := fs.DirOrFileByPath(pth)
	if err != nil {
		return "", err
	}
	indexer := vfs.NewCouchdbIndexer(inst)
	var dirID string
	if d != nil {
		dirID = d.DirID
	} else {
		dirID = f.DirID
	}
	name := conflictName(indexer, dirID, path.Base(pth), f != nil)
	if s.Owner {
		return name, nil
	}
	if d != nil {
		old := d.Clone().(*vfs.DirDoc)
		d.DocName = name
		d.Fullpath = path.Join(path.Dir(d.Fullpath), d.DocName)
		return "", fs.UpdateDirDoc(old, d)
	}
	old := f.Clone().(*vfs.FileDoc)
	f.DocName = name
	f.ResetFullpath()
	return "", fs.UpdateFileDoc(old, f)
}

// getDirDocFromInstance fetches informations about a directory from the given
// member of the sharing.
func (s *Sharing) getDirDocFromInstance(inst *instance.Instance, m *Member, creds *Credentials, dirID string) (*vfs.DirDoc, error) {
	if creds == nil || creds.AccessToken == nil {
		return nil, ErrInvalidSharing
	}
	u, err := url.Parse(m.Instance)
	if err != nil {
		return nil, ErrInvalidSharing
	}
	opts := &request.Options{
		Method: http.MethodGet,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/io.cozy.files/" + dirID,
		Headers: request.Headers{
			"Accept":        "application/json",
			"Authorization": "Bearer " + creds.AccessToken.AccessToken,
		},
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, res, err, s, m, creds, opts, nil)
	}
	if err != nil {
		if res != nil && res.StatusCode/100 == 5 {
			return nil, ErrInternalServerError
		}
		return nil, err
	}
	defer res.Body.Close()
	var doc *vfs.DirDoc
	if err = json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// getDirDocFromNetwork fetches informations about a directory from the other
// cozy instances of this sharing.
func (s *Sharing) getDirDocFromNetwork(inst *instance.Instance, dirID string) (*vfs.DirDoc, error) {
	if !s.Owner {
		return s.getDirDocFromInstance(inst, &s.Members[0], &s.Credentials[0], dirID)
	}
	for i := range s.Credentials {
		doc, err := s.getDirDocFromInstance(inst, &s.Members[i+1], &s.Credentials[i], dirID)
		if err == nil {
			return doc, nil
		}
	}
	return nil, ErrFolderNotFound
}

// recreateParent is used when a file or folder is added by a cozy, and sent to
// this instance, but its parent directory was trashed and deleted on this
// cozy. To resolve the conflict, this instance will fetch informations from
// the other instance about the parent directory and will recreate it. It can
// be necessary to recurse if there were several levels of directories deleted.
func (s *Sharing) recreateParent(inst *instance.Instance, dirID string) (*vfs.DirDoc, error) {
	inst.Logger().WithNamespace("replicator").
		Debugf("Recreate parent dirID=%s", dirID)
	doc, err := s.getDirDocFromNetwork(inst, dirID)
	if err != nil {
		return nil, fmt.Errorf("recreateParent: %w", err)
	}
	fs := inst.VFS()
	var parent *vfs.DirDoc
	if doc.DirID == "" {
		parent, err = s.GetSharingDir(inst)
	} else {
		parent, err = fs.DirByID(doc.DirID)
		if errors.Is(err, os.ErrNotExist) {
			parent, err = s.recreateParent(inst, doc.DirID)
		}
	}
	if err != nil {
		return nil, err
	}
	doc.DirID = parent.DocID
	doc.Fullpath = path.Join(parent.Fullpath, doc.DocName)
	doc.SetRev("")
	err = fs.CreateDir(doc)
	if err != nil {
		// Maybe the directory has been created concurrently, so let's try
		// again to fetch it from the database
		if errors.Is(err, os.ErrExist) {
			return fs.DirByID(dirID)
		}
		return nil, fmt.Errorf("recreateParent: %w", err)
	}
	return doc, nil
}

// extractNameAndIndexer takes a target document, extracts the name and creates
// a sharing indexer with _rev and _revisions
func extractNameAndIndexer(inst *instance.Instance, target map[string]interface{}, ref *SharedRef) (string, *sharingIndexer, error) {
	name, ok := target["name"].(string)
	if !ok {
		inst.Logger().WithNamespace("replicator").
			Warnf("Missing name for directory %#v", target)
		return "", nil, ErrInternalServerError
	}
	rev, ok := target["_rev"].(string)
	if !ok {
		inst.Logger().WithNamespace("replicator").
			Warnf("Missing _rev for directory %#v", target)
		return "", nil, ErrInternalServerError
	}
	revs := revsMapToStruct(target["_revisions"])
	if revs == nil {
		inst.Logger().WithNamespace("replicator").
			Warnf("Invalid _revisions for directory %#v", target)
		return "", nil, ErrInternalServerError
	}
	indexer := newSharingIndexer(inst, &bulkRevs{
		Rev:       rev,
		Revisions: *revs,
	}, ref)
	return name, indexer, nil
}

type nameConflictResolution int

const (
	delayResolution nameConflictResolution = iota
	resolveResolution
)

// CreateDir creates a directory on this cozy to reflect a change on another
// cozy instance of this sharing.
func (s *Sharing) CreateDir(inst *instance.Instance, target map[string]interface{}, resolution nameConflictResolution) error {
	inst.Logger().WithNamespace("replicator").
		Debugf("CreateDir %v (%#v)", target["_id"], target)
	ref := SharedRef{
		Infos: make(map[string]SharedInfo),
	}
	name, indexer, err := extractNameAndIndexer(inst, target, &ref)
	if err != nil {
		return err
	}
	fs := inst.VFS().UseSharingIndexer(indexer)

	var parent *vfs.DirDoc
	if dirID, ok := target["dir_id"].(string); ok {
		parent, err = fs.DirByID(dirID)
		if errors.Is(err, os.ErrNotExist) {
			parent, err = s.recreateParent(inst, dirID)
		}
		if err != nil {
			inst.Logger().WithNamespace("replicator").
				Debugf("Conflict for parent on creating dir: %s", err)
			return err
		}
	} else {
		parent, err = s.GetSharingDir(inst)
		if err != nil {
			return err
		}
	}

	dir, err := vfs.NewDirDocWithParent(name, parent, nil)
	if err != nil {
		inst.Logger().WithNamespace("replicator").
			Warnf("Cannot initialize dir doc: %s", err)
		return err
	}
	dir.SetID(target["_id"].(string))
	ref.SID = consts.Files + "/" + dir.DocID
	copySafeFieldsToDir(target, dir)
	rule, ruleIndex := s.findRuleForNewDirectory(dir)
	if rule == nil {
		return ErrSafety
	}
	ref.Infos[s.SID] = SharedInfo{Rule: ruleIndex}
	err = fs.CreateDir(dir)
	if errors.Is(err, os.ErrExist) && resolution == resolveResolution {
		name, errr := s.resolveConflictSamePath(inst, dir.DocID, dir.Fullpath)
		if errr != nil {
			return errr
		}
		if name != "" {
			indexer.IncrementRevision()
			dir.DocName = name
			dir.Fullpath = path.Join(path.Dir(dir.Fullpath), dir.DocName)
		}
		err = fs.CreateDir(dir)
	}
	if err != nil {
		inst.Logger().WithNamespace("replicator").
			Debugf("Cannot create dir: %s", err)
		return err
	}
	return nil
}

// prepareDirWithAncestors find the parent directory for dir, and recreates it
// if it is missing.
func (s *Sharing) prepareDirWithAncestors(inst *instance.Instance, dir *vfs.DirDoc, dirID string) error {
	if dirID == "" {
		parent, err := s.GetSharingDir(inst)
		if err != nil {
			return err
		}
		dir.DirID = parent.DocID
		dir.Fullpath = path.Join(parent.Fullpath, dir.DocName)
	} else if dirID != dir.DirID {
		parent, err := inst.VFS().DirByID(dirID)
		if errors.Is(err, os.ErrNotExist) {
			parent, err = s.recreateParent(inst, dirID)
		}
		if err != nil {
			inst.Logger().WithNamespace("replicator").
				Debugf("Conflict for parent on updating dir: %s", err)
			return err
		}
		dir.DirID = parent.DocID
		dir.Fullpath = path.Join(parent.Fullpath, dir.DocName)
	} else {
		dir.Fullpath = path.Join(path.Dir(dir.Fullpath), dir.DocName)
	}
	return nil
}

// UpdateDir updates a directory on this cozy to reflect a change on another
// cozy instance of this sharing.
func (s *Sharing) UpdateDir(
	inst *instance.Instance,
	target map[string]interface{},
	dir *vfs.DirDoc,
	ref *SharedRef,
	resolution nameConflictResolution,
) error {
	inst.Logger().WithNamespace("replicator").
		Debugf("UpdateDir %v (%#v)", target["_id"], target)
	if strings.HasPrefix(dir.Fullpath+"/", vfs.TrashDirName+"/") {
		// Don't update a directory in the trash
		return nil
	}

	name, indexer, err := extractNameAndIndexer(inst, target, ref)
	if err != nil {
		return err
	}

	chain := revsStructToChain(indexer.bulkRevs.Revisions)
	conflict := detectConflict(dir.DocRev, chain)
	switch conflict {
	case LostConflict:
		return nil
	case WonConflict:
		indexer.WillResolveConflict(dir.DocRev, chain)
	case NoConflict:
		// Nothing to do
	}

	fs := inst.VFS().UseSharingIndexer(indexer)
	oldDoc := dir.Clone().(*vfs.DirDoc)
	dir.DocName = name
	dirID, _ := target["dir_id"].(string)
	if err = s.prepareDirWithAncestors(inst, dir, dirID); err != nil {
		return err
	}
	copySafeFieldsToDir(target, dir)

	err = fs.UpdateDirDoc(oldDoc, dir)
	if errors.Is(err, os.ErrExist) && resolution == resolveResolution {
		name, errb := s.resolveConflictSamePath(inst, dir.DocID, dir.Fullpath)
		if errb != nil {
			return errb
		}
		if name != "" {
			indexer.IncrementRevision()
			dir.DocName = name
			dir.Fullpath = path.Join(path.Dir(dir.Fullpath), dir.DocName)
		}
		err = fs.UpdateDirDoc(oldDoc, dir)
	}
	if err != nil {
		inst.Logger().WithNamespace("replicator").
			Debugf("Cannot update dir: %s", err)
		return err
	}
	return nil
}

// TrashDir puts the directory in the trash
func (s *Sharing) TrashDir(inst *instance.Instance, dir *vfs.DirDoc) error {
	inst.Logger().WithNamespace("replicator").
		Debugf("TrashDir %s (%#v)", dir.DocID, dir)
	if strings.HasPrefix(dir.Fullpath+"/", vfs.TrashDirName+"/") {
		// nothing to do if the directory is already in the trash
		return nil
	}

	newdir := dir.Clone().(*vfs.DirDoc)
	if newdir.CozyMetadata == nil {
		newdir.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
	} else {
		newdir.CozyMetadata.UpdatedAt = time.Now()
	}

	newdir.DirID = consts.TrashDirID
	fs := inst.VFS()
	exists, err := fs.DirChildExists(newdir.DirID, newdir.DocName)
	if err != nil {
		return fmt.Errorf("Sharing.TrashDir: %w", err)
	}
	if exists {
		newdir.DocName = conflictName(fs, newdir.DirID, newdir.DocName, true)
	}
	newdir.Fullpath = path.Join(vfs.TrashDirName, newdir.DocName)
	newdir.RestorePath = path.Dir(dir.Fullpath)
	if err := s.dissociateDir(inst, dir, newdir); err != nil {
		return fmt.Errorf("Sharing.TrashDir: %w", err)
	}
	return nil
}

func (s *Sharing) dissociateDir(inst *instance.Instance, olddoc, newdoc *vfs.DirDoc) error {
	fs := inst.VFS()

	newdoc.SetID("")
	newdoc.SetRev("")
	if err := fs.DissociateDir(olddoc, newdoc); err != nil {
		newdoc.DocName = conflictName(fs, newdoc.DirID, newdoc.DocName, true)
		newdoc.Fullpath = path.Join(path.Dir(newdoc.Fullpath), newdoc.DocName)
		if err := fs.DissociateDir(olddoc, newdoc); err != nil {
			return err
		}
	}

	sid := olddoc.DocType() + "/" + olddoc.ID()
	var ref SharedRef
	if err := couchdb.GetDoc(inst, consts.Shared, sid, &ref); err == nil {
		if s.Owner {
			ref.Revisions.Add(olddoc.Rev())
			ref.Infos[s.SID] = SharedInfo{
				Rule:        ref.Infos[s.SID].Rule,
				Binary:      false,
				Removed:     true,
				Dissociated: true,
			}
			_ = couchdb.UpdateDoc(inst, &ref)
		} else {
			_ = couchdb.DeleteDoc(inst, &ref)
		}
	}

	var errm error
	iter := fs.DirIterator(olddoc, nil)
	for {
		d, f, err := iter.Next()
		if errors.Is(err, vfs.ErrIteratorDone) {
			break
		}
		if err != nil {
			return err
		}
		if f != nil {
			newf := f.Clone().(*vfs.FileDoc)
			newf.DirID = newdoc.DocID
			newf.Trashed = true
			newf.ResetFullpath()
			err = s.dissociateFile(inst, f, newf)
		} else {
			newd := d.Clone().(*vfs.DirDoc)
			newd.DirID = newdoc.DocID
			newd.Fullpath = path.Join(newdoc.Fullpath, newd.DocName)
			err = s.dissociateDir(inst, d, newd)
		}
		if err != nil {
			errm = multierror.Append(errm, err)
		}
	}
	return errm
}

// TrashFile puts the file in the trash (except if the file has a reference, in
// which case, we keep it in a special folder)
func (s *Sharing) TrashFile(inst *instance.Instance, file *vfs.FileDoc, rule *Rule) error {
	inst.Logger().WithNamespace("replicator").
		Debugf("TrashFile %s (%#v)", file.DocID, file)
	if file.Trashed {
		// Nothing to do if the file is already in the trash
		return nil
	}
	if file.CozyMetadata == nil {
		file.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
	} else {
		file.CozyMetadata.UpdatedAt = time.Now()
	}
	olddoc := file.Clone().(*vfs.FileDoc)
	removeReferencesFromRule(file, rule)
	if s.Owner && rule.Selector == couchdb.SelectorReferencedBy {
		// Do not move/trash photos removed from an album for the owner
		if err := s.dissociateFile(inst, olddoc, file); err != nil {
			return fmt.Errorf("Sharing.TrashFile: %w", err)
		}
		return nil
	}
	if len(file.ReferencedBy) == 0 {
		oldpath, err := olddoc.Path(inst.VFS())
		if err != nil {
			return err
		}
		file.RestorePath = path.Dir(oldpath)
		file.Trashed = true
		file.DirID = consts.TrashDirID
		file.ResetFullpath()
		if err := s.dissociateFile(inst, olddoc, file); err != nil {
			return fmt.Errorf("Sharing.TrashFile: %w", err)
		}
		return nil
	}
	parent, err := s.GetNoLongerSharedDir(inst)
	if err != nil {
		return fmt.Errorf("Sharing.TrashFile: %w", err)
	}
	file.DirID = parent.DocID
	file.ResetFullpath()
	if err := s.dissociateFile(inst, olddoc, file); err != nil {
		return fmt.Errorf("Sharing.TrashFile: %w", err)
	}
	return nil
}

func (s *Sharing) dissociateFile(inst *instance.Instance, olddoc, newdoc *vfs.FileDoc) error {
	fs := inst.VFS()

	newdoc.SetID("")
	newdoc.SetRev("")
	if err := fs.DissociateFile(olddoc, newdoc); err != nil {
		newdoc.DocName = conflictName(fs, newdoc.DirID, newdoc.DocName, true)
		newdoc.ResetFullpath()
		if err := fs.DissociateFile(olddoc, newdoc); err != nil {
			return err
		}
	}

	sid := olddoc.DocType() + "/" + olddoc.ID()
	var ref SharedRef
	if err := couchdb.GetDoc(inst, consts.Shared, sid, &ref); err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	if !s.Owner {
		return couchdb.DeleteDoc(inst, &ref)
	}
	ref.Revisions.Add(olddoc.Rev())
	ref.Infos[s.SID] = SharedInfo{
		Rule:        ref.Infos[s.SID].Rule,
		Binary:      false,
		Removed:     true,
		Dissociated: true,
	}
	return couchdb.UpdateDoc(inst, &ref)
}

func dirToJSONDoc(dir *vfs.DirDoc, instanceURL string) couchdb.JSONDoc {
	doc := couchdb.JSONDoc{
		Type: consts.Files,
		M: map[string]interface{}{
			"type":                       dir.Type,
			"_id":                        dir.DocID,
			"_rev":                       dir.DocRev,
			"name":                       dir.DocName,
			"created_at":                 dir.CreatedAt,
			"updated_at":                 dir.UpdatedAt,
			"tags":                       dir.Tags,
			"path":                       dir.Fullpath,
			couchdb.SelectorReferencedBy: dir.ReferencedBy,
		},
	}
	if dir.DirID != "" {
		doc.M["dir_id"] = dir.DirID
	}
	if dir.RestorePath != "" {
		doc.M["restore_path"] = dir.RestorePath
	}
	if len(dir.Metadata) > 0 {
		doc.M["metadata"] = dir.Metadata.RemoveCertifiedMetadata()
	}
	fcm := dir.CozyMetadata
	if fcm == nil {
		fcm = vfs.NewCozyMetadata(instanceURL)
		fcm.CreatedAt = dir.CreatedAt
		fcm.UpdatedAt = dir.UpdatedAt
	}
	doc.M["cozyMetadata"] = fcm.ToJSONDoc()
	return doc
}

func fileToJSONDoc(file *vfs.FileDoc, instanceURL string) couchdb.JSONDoc {
	doc := couchdb.JSONDoc{
		Type: consts.Files,
		M: map[string]interface{}{
			"type":                       file.Type,
			"_id":                        file.DocID,
			"_rev":                       file.DocRev,
			"name":                       file.DocName,
			"created_at":                 file.CreatedAt,
			"updated_at":                 file.UpdatedAt,
			"size":                       file.ByteSize,
			"md5sum":                     file.MD5Sum,
			"mime":                       file.Mime,
			"class":                      file.Class,
			"executable":                 file.Executable,
			"trashed":                    file.Trashed,
			"tags":                       file.Tags,
			couchdb.SelectorReferencedBy: file.ReferencedBy,
		},
	}
	if file.DirID != "" {
		doc.M["dir_id"] = file.DirID
	}
	if file.RestorePath != "" {
		doc.M["restore_path"] = file.RestorePath
	}
	if len(file.Metadata) > 0 {
		meta := file.Metadata
		meta = meta.RemoveCertifiedMetadata()
		meta = meta.RemoveFavoriteMetadata()
		doc.M["metadata"] = meta
	}
	fcm := file.CozyMetadata
	if fcm == nil {
		fcm = vfs.NewCozyMetadata(instanceURL)
		fcm.CreatedAt = file.CreatedAt
		fcm.UpdatedAt = file.UpdatedAt
		uploadedAt := file.CreatedAt
		fcm.UploadedAt = &uploadedAt
		fcm.UploadedOn = instanceURL
	}
	doc.M["cozyMetadata"] = fcm.ToJSONDoc()
	return doc
}
