package vfs

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// DirDoc is a struct containing all the informations about a
// directory. It implements the couchdb.Doc and jsonapi.Object
// interfaces.
type DirDoc struct {
	// Type of document. Useful to (de)serialize and filter the data
	// from couch.
	Type string `json:"type"`
	// Qualified file identifier
	DocID string `json:"_id,omitempty"`
	// Directory revision
	DocRev string `json:"_rev,omitempty"`
	// Directory name
	DocName string `json:"name,omitempty"`
	// Parent directory identifier
	DirID       string `json:"dir_id,omitempty"`
	RestorePath string `json:"restore_path,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Tags      []string  `json:"tags,omitempty"`

	// Directory path on VFS.
	// Fullpath should always be present. It is marked "omitempty" because
	// DirDoc is the base of the DirOrFile struct.
	Fullpath string `json:"path,omitempty"`

	ReferencedBy      []couchdb.DocReference `json:"referenced_by,omitempty"`
	NotSynchronizedOn []couchdb.DocReference `json:"not_synchronized_on,omitempty"`

	Metadata     Metadata           `json:"metadata,omitempty"`
	CozyMetadata *FilesCozyMetadata `json:"cozyMetadata,omitempty"`
}

// ID returns the directory qualified identifier
func (d *DirDoc) ID() string { return d.DocID }

// Rev returns the directory revision
func (d *DirDoc) Rev() string { return d.DocRev }

// DocType returns the directory document type
func (d *DirDoc) DocType() string { return consts.Files }

// Clone implements couchdb.Doc
func (d *DirDoc) Clone() couchdb.Doc {
	cloned := *d
	cloned.Tags = make([]string, len(d.Tags))
	copy(cloned.Tags, d.Tags)
	cloned.ReferencedBy = make([]couchdb.DocReference, len(d.ReferencedBy))
	copy(cloned.ReferencedBy, d.ReferencedBy)
	cloned.NotSynchronizedOn = make([]couchdb.DocReference, len(d.NotSynchronizedOn))
	copy(cloned.NotSynchronizedOn, d.NotSynchronizedOn)
	cloned.Metadata = make(Metadata, len(d.Metadata))
	for k, v := range d.Metadata {
		cloned.Metadata[k] = v
	}
	if d.CozyMetadata != nil {
		cloned.CozyMetadata = d.CozyMetadata.Clone()
	}
	return &cloned
}

// SetID changes the directory qualified identifier
func (d *DirDoc) SetID(id string) { d.DocID = id }

// SetRev changes the directory revision
func (d *DirDoc) SetRev(rev string) { d.DocRev = rev }

// Path is used to generate the file path
func (d *DirDoc) Path(fs FilePather) (string, error) {
	return d.Fullpath, nil
}

// Parent returns the parent directory document
func (d *DirDoc) Parent(fs Indexer) (*DirDoc, error) {
	parent, err := fs.DirByID(d.DirID)
	if os.IsNotExist(err) {
		err = ErrParentDoesNotExist
	}
	return parent, err
}

// Name returns base name of the file
func (d *DirDoc) Name() string { return d.DocName }

// Size returns the length in bytes for regular files; system-dependent for others
func (d *DirDoc) Size() int64 { return 0 }

// Mode returns the file mode bits
func (d *DirDoc) Mode() os.FileMode { return 0755 }

// ModTime returns the modification time
func (d *DirDoc) ModTime() time.Time { return d.UpdatedAt }

// IsDir returns the abbreviation for Mode().IsDir()
func (d *DirDoc) IsDir() bool { return true }

// Sys returns the underlying data source (can return nil)
func (d *DirDoc) Sys() interface{} { return nil }

// IsEmpty returns whether or not the directory has at least one child.
func (d *DirDoc) IsEmpty(fs VFS) (bool, error) {
	iter := fs.DirIterator(d, &IteratorOptions{ByFetch: 1})
	_, _, err := iter.Next()
	if errors.Is(err, ErrIteratorDone) {
		return true, nil
	}
	return false, err
}

// AddReferencedBy adds referenced_by to the directory
func (d *DirDoc) AddReferencedBy(ri ...couchdb.DocReference) {
	for _, ref := range ri {
		if !containsDocReference(d.ReferencedBy, ref) {
			d.ReferencedBy = append(d.ReferencedBy, ref)
		}
	}
}

// RemoveReferencedBy removes one or several referenced_by to the directory
func (d *DirDoc) RemoveReferencedBy(ri ...couchdb.DocReference) {
	// https://github.com/golang/go/wiki/SliceTricks#filtering-without-allocating
	referenced := d.ReferencedBy[:0]
	for _, ref := range d.ReferencedBy {
		if !containsDocReference(ri, ref) {
			referenced = append(referenced, ref)
		}
	}
	d.ReferencedBy = referenced
}

// AddNotSynchronizedOn adds not_synchronized_on to the directory
func (d *DirDoc) AddNotSynchronizedOn(refs ...couchdb.DocReference) {
	for _, ref := range refs {
		if !containsDocReference(d.NotSynchronizedOn, ref) {
			d.NotSynchronizedOn = append(d.NotSynchronizedOn, ref)
		}
	}
}

// RemoveNotSynchronizedOn removes one or several not_synchronized_on to the
// directory
func (d *DirDoc) RemoveNotSynchronizedOn(refs ...couchdb.DocReference) {
	// https://github.com/golang/go/wiki/SliceTricks#filtering-without-allocating
	references := d.NotSynchronizedOn[:0]
	for _, ref := range d.NotSynchronizedOn {
		if !containsDocReference(refs, ref) {
			references = append(references, ref)
		}
	}
	d.NotSynchronizedOn = references
}

func (d *DirDoc) SharingID() string {
	for _, ref := range d.ReferencedBy {
		if ref.Type == consts.Sharings {
			return ref.ID
		}
	}
	return ""
}

// NewDirDoc is the DirDoc constructor. The given name is validated.
func NewDirDoc(index Indexer, name, dirID string, tags []string) (*DirDoc, error) {
	if dirID == "" {
		dirID = consts.RootDirID
	}

	var dirPath string
	if dirID == consts.RootDirID {
		dirPath = "/"
	} else {
		parent, err := index.DirByID(dirID)
		if err != nil {
			return nil, err
		}
		dirPath = parent.Fullpath
	}

	return NewDirDocWithPath(name, dirID, dirPath, tags)
}

// NewDirDocWithParent returns an instance of DirDoc from a parent document.
// The given name is validated.
func NewDirDocWithParent(name string, parent *DirDoc, tags []string) (*DirDoc, error) {
	return NewDirDocWithPath(name, parent.DocID, parent.Fullpath, tags)
}

// NewDirDocWithPath returns an instance of DirDoc its directory ID and path.
// The given name is validated.
func NewDirDocWithPath(name, dirID, dirPath string, tags []string) (*DirDoc, error) {
	if err := checkFileName(name); err != nil {
		return nil, err
	}

	if dirPath == "" || dirPath == "." {
		dirPath = "/"
	}
	fullpath := path.Join(dirPath, name)
	if err := checkDepth(fullpath); err != nil {
		return nil, err
	}

	createDate := time.Now()
	return &DirDoc{
		Type:    consts.DirType,
		DocName: name,
		DirID:   dirID,

		CreatedAt: createDate,
		UpdatedAt: createDate,
		Tags:      uniqueTags(tags),
		Fullpath:  fullpath,
	}, nil
}

// ModifyDirMetadata modify the metadata associated to a directory. It
// can be used to rename or move the directory in the VFS.
func ModifyDirMetadata(fs VFS, olddoc *DirDoc, patch *DocPatch) (*DirDoc, error) {
	id := olddoc.ID()
	if id == consts.RootDirID || id == consts.TrashDirID {
		return nil, os.ErrInvalid
	}

	var oldFavorite *bool
	if olddoc.CozyMetadata != nil {
		oldFavorite = &olddoc.CozyMetadata.Favorite
	}

	var err error
	cdate := olddoc.CreatedAt
	patch, err = normalizeDocPatch(&DocPatch{
		Name:         &olddoc.DocName,
		DirID:        &olddoc.DirID,
		RestorePath:  &olddoc.RestorePath,
		Tags:         &olddoc.Tags,
		UpdatedAt:    &olddoc.UpdatedAt,
		CozyMetadata: CozyMetadataPatch{Favorite: oldFavorite},
	}, patch, cdate)

	if err != nil {
		return nil, err
	}

	var newdoc *DirDoc
	if *patch.DirID != olddoc.DirID {
		if strings.HasPrefix(olddoc.Fullpath, TrashDirName) {
			return nil, ErrFileInTrash
		}
		newdoc, err = NewDirDoc(fs, *patch.Name, *patch.DirID, *patch.Tags)
	} else {
		newdoc, err = NewDirDocWithPath(*patch.Name, olddoc.DirID, path.Dir(olddoc.Fullpath), *patch.Tags)
	}
	if err != nil {
		return nil, err
	}

	newdoc.RestorePath = *patch.RestorePath
	newdoc.CreatedAt = cdate
	newdoc.UpdatedAt = *patch.UpdatedAt
	newdoc.ReferencedBy = olddoc.ReferencedBy
	newdoc.NotSynchronizedOn = olddoc.NotSynchronizedOn
	newdoc.Metadata = olddoc.Metadata
	if patch.Metadata != nil {
		newdoc.Metadata = *patch.Metadata
	}
	newdoc.CozyMetadata = olddoc.CozyMetadata
	if newdoc.CozyMetadata != nil && patch.CozyMetadata.Favorite != nil {
		newdoc.CozyMetadata.Favorite = *patch.CozyMetadata.Favorite
	}

	if err = fs.UpdateDirDoc(olddoc, newdoc); err != nil {
		return nil, err
	}
	return newdoc, nil
}

// TrashDir is used to delete a directory given its document
func TrashDir(fs VFS, olddoc *DirDoc) (*DirDoc, error) {
	oldpath, err := olddoc.Path(fs)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(oldpath, TrashDirName) {
		return nil, ErrFileInTrash
	}

	trashDirID := consts.TrashDirID
	restorePath := path.Dir(oldpath)

	var newdoc *DirDoc
	err = tryOrUseSuffix(olddoc.DocName, conflictFormat, func(name string) error {
		newdoc = olddoc.Clone().(*DirDoc)
		newdoc.DirID = trashDirID
		newdoc.RestorePath = restorePath
		newdoc.DocName = name
		newdoc.Fullpath = path.Join(TrashDirName, name)
		newdoc.CozyMetadata = olddoc.CozyMetadata
		return fs.UpdateDirDoc(olddoc, newdoc)
	})
	if err != nil {
		return nil, err
	}
	return newdoc, nil
}

// RestoreDir is used to restore a trashed directory given its document
func RestoreDir(fs VFS, olddoc *DirDoc) (*DirDoc, error) {
	oldpath, err := olddoc.Path(fs)
	if err != nil {
		return nil, err
	}

	restoreDir, err := getRestoreDir(fs, oldpath, olddoc.RestorePath)
	if err != nil {
		return nil, err
	}

	name := stripConflictSuffix(olddoc.DocName)

	var newdoc *DirDoc
	err = tryOrUseSuffix(name, conflictFormat, func(name string) error {
		newdoc = olddoc.Clone().(*DirDoc)
		newdoc.DirID = restoreDir.DocID
		newdoc.RestorePath = ""
		newdoc.DocName = name
		newdoc.Fullpath = path.Join(restoreDir.Fullpath, name)
		newdoc.CozyMetadata = olddoc.CozyMetadata
		return fs.UpdateDirDoc(olddoc, newdoc)
	})
	if err != nil {
		return nil, err
	}

	return newdoc, nil
}

// FilterNotSynchronizedDocs filters a changes feed to replace documents in
// not_synchronized_on directories with deleted: true entries.
func FilterNotSynchronizedDocs(fs VFS, clientID string, changes *couchdb.ChangesResponse) error {
	if len(changes.Results) == 0 {
		return nil
	}

	notSynchronizedDirs, err := fetchNotSynchronizedOn(fs, clientID)
	if err != nil {
		return err
	}
	if len(notSynchronizedDirs.byID) == 0 {
		return nil
	}

	fp := NewFilePatherWithCache(fs.GetIndexer())
	for i := range changes.Results {
		doc := changes.Results[i].Doc
		if isNotSynchronized(fp, notSynchronizedDirs, doc) {
			var rev string
			if len(changes.Results[i].Changes) > 0 {
				rev = changes.Results[i].Changes[0].Rev
			}
			docID := changes.Results[i].DocID
			changes.Results[i].Doc = couchdb.JSONDoc{
				M: map[string]interface{}{
					"_id":      docID,
					"_rev":     rev,
					"_deleted": true,
				},
				Type: consts.Files,
			}
			changes.Results[i].Deleted = true
		}
	}

	return nil
}

type notSynchronizedMap struct {
	byID   map[string]struct{}
	byPath map[string]struct{}
}

func fetchNotSynchronizedOn(fs VFS, clientID string) (notSynchronizedMap, error) {
	m := notSynchronizedMap{}
	docs, err := fs.ListNotSynchronizedOn(clientID)
	if err != nil || len(docs) == 0 {
		return m, err
	}

	m.byID = make(map[string]struct{})
	m.byPath = make(map[string]struct{})
	for _, doc := range docs {
		m.byID[doc.DocID] = struct{}{}
		m.byPath[doc.Fullpath] = struct{}{}
	}
	return m, nil
}

func isNotSynchronized(fp FilePather, notSynchronizedDirs notSynchronizedMap, doc couchdb.JSONDoc) bool {
	docID := doc.ID()
	if _, ok := notSynchronizedDirs.byID[docID]; ok {
		return true
	}

	fpath, _ := doc.M["path"].(string)
	if doc.M["type"] == consts.FileType {
		dirID, _ := doc.M["dir_id"].(string)
		fpath, _ = fp.FilePath(&FileDoc{DocID: docID, DirID: dirID})
	}

	for {
		if _, ok := notSynchronizedDirs.byPath[fpath]; ok {
			return true
		}
		if fpath == "" || fpath == "/" {
			return false
		}
		fpath = filepath.Dir(fpath)
	}
}

var (
	_ couchdb.Doc = &DirDoc{}
	_ os.FileInfo = &DirDoc{}
)
