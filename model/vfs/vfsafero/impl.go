// Package vfsafero is the implementation of the Virtual File System by using
// afero. Afero is a library for manipulating files and directory on the local
// file system.
package vfsafero

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/filetype"
	"github.com/cozy/cozy-stack/pkg/lock"

	"github.com/spf13/afero"
)

var memfsMap sync.Map

// aferoVFS is a struct implementing the vfs.VFS interface associated with
// an afero.Fs filesystem. The indexing of the elements of the filesystem is
// done in couchdb.
type aferoVFS struct {
	vfs.Indexer
	vfs.DiskThresholder

	cluster int
	domain  string
	prefix  string
	context string
	fs      afero.Fs
	mu      lock.ErrorRWLocker
	pth     string

	// whether or not the localfilesystem requires an initialisation of its root
	// directory
	osFS bool
}

// GetMemFS returns a file system in memory for the given key
func GetMemFS(key string) afero.Fs {
	val, ok := memfsMap.Load(key)
	if !ok {
		val, _ = memfsMap.LoadOrStore(key, afero.NewMemMapFs())
	}
	return val.(afero.Fs)
}

// New returns a vfs.VFS instance associated with the specified indexer and
// storage url.
//
// The supported scheme of the storage url are file://, for an OS-FS store, and
// mem:// for an in-memory store. The backend used is the afero package.
func New(db vfs.Prefixer, index vfs.Indexer, disk vfs.DiskThresholder, mu lock.ErrorRWLocker, fsURL *url.URL, pathSegment string) (vfs.VFS, error) {
	if fsURL.Scheme != "mem" && fsURL.Path == "" {
		return nil, fmt.Errorf("vfsafero: please check the supplied fs url: %s",
			fsURL.String())
	}
	if pathSegment == "" {
		return nil, fmt.Errorf("vfsafero: specified path segment is empty")
	}
	pth := path.Join(fsURL.Path, pathSegment)
	var fs afero.Fs
	switch fsURL.Scheme {
	case "file":
		fs = afero.NewBasePathFs(afero.NewOsFs(), pth)
	case "mem":
		fs = GetMemFS(db.DomainName())
	default:
		return nil, fmt.Errorf("vfsafero: non supported scheme %s", fsURL.Scheme)
	}
	return &aferoVFS{
		Indexer:         index,
		DiskThresholder: disk,

		cluster: db.DBCluster(),
		domain:  db.DomainName(),
		prefix:  db.DBPrefix(),
		context: db.GetContextName(),
		fs:      fs,
		mu:      mu,
		pth:     pth,
		// for now, only the file:// scheme needs a specific initialisation of its
		// root directory.
		osFS: fsURL.Scheme == "file",
	}, nil
}

func (afs *aferoVFS) MaxFileSize() int64 {
	return -1 // no limit
}

func (afs *aferoVFS) DBCluster() int {
	return afs.cluster
}

func (afs *aferoVFS) DomainName() string {
	return afs.domain
}

func (afs *aferoVFS) DBPrefix() string {
	return afs.prefix
}

func (afs *aferoVFS) GetContextName() string {
	return afs.context
}

func (afs *aferoVFS) GetIndexer() vfs.Indexer {
	return afs.Indexer
}

func (afs *aferoVFS) UseSharingIndexer(index vfs.Indexer) vfs.VFS {
	return &aferoVFS{
		Indexer:         index,
		DiskThresholder: afs.DiskThresholder,
		domain:          afs.domain,
		prefix:          afs.prefix,
		fs:              afs.fs,
		mu:              afs.mu,
		pth:             afs.pth,
		osFS:            afs.osFS,
	}
}

// Init creates the root directory document and the trash directory for this
// file system.
func (afs *aferoVFS) InitFs() error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	if err := afs.Indexer.InitIndex(); err != nil {
		return err
	}
	// for a file:// fs, we need to create the root directory container
	if afs.osFS {
		if err := afero.NewOsFs().MkdirAll(afs.pth, 0755); err != nil {
			return err
		}
	}
	if err := afs.fs.Mkdir(vfs.TrashDirName, 0755); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// Delete removes all the elements associated with the filesystem.
func (afs *aferoVFS) Delete() error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	if afs.osFS {
		return afero.NewOsFs().RemoveAll(afs.pth)
	}
	return nil
}

func (afs *aferoVFS) CreateDir(doc *vfs.DirDoc) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	err := afs.fs.Mkdir(doc.Fullpath, 0755)
	if err != nil {
		return err
	}
	if doc.ID() == "" {
		err = afs.Indexer.CreateDirDoc(doc)
	} else {
		err = afs.Indexer.CreateNamedDirDoc(doc)
	}
	if err != nil {
		_ = afs.fs.Remove(doc.Fullpath)
	}
	return err
}

func (afs *aferoVFS) CreateFile(newdoc, olddoc *vfs.FileDoc, opts ...vfs.CreateOptions) (vfs.File, error) {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return nil, lockerr
	}
	defer afs.mu.Unlock()

	newsize, maxsize, capsize, err := vfs.CheckAvailableDiskSpace(afs, newdoc)
	if err != nil {
		return nil, err
	}

	if olddoc != nil {
		newdoc.SetID(olddoc.ID())
		newdoc.SetRev(olddoc.Rev())
		newdoc.CreatedAt = olddoc.CreatedAt
	}

	newpath, err := afs.Indexer.FilePath(newdoc)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(newpath, vfs.TrashDirName+"/") {
		if !vfs.OptionsAllowCreationInTrash(opts) {
			return nil, vfs.ErrParentInTrash
		}
	}

	if olddoc == nil {
		exists, err := afs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, os.ErrExist
		}
	}

	f, err := afero.TempFile(afs.fs, "/", newdoc.DocName)
	if err != nil {
		return nil, err
	}
	tmppath := path.Join("/", f.Name())

	hash := md5.New()
	extractor := vfs.NewMetaExtractor(newdoc)

	return &aferoFileCreation{
		afs:     afs,
		f:       f,
		newdoc:  newdoc,
		olddoc:  olddoc,
		tmppath: tmppath,
		w:       0,
		size:    newsize,
		maxsize: maxsize,
		capsize: capsize,
		hash:    hash,
		meta:    extractor,
	}, nil
}

func (afs *aferoVFS) CopyFile(olddoc, newdoc *vfs.FileDoc) (err error) {
	var newfile *aferoFileCreation
	defer func() {
		// XXX: we need to release the VFS lock before closing newfile as
		// aferoFileCreation.Close requests its own lock.
		// Therefore, this defer method needs to come before the afs.mu.Unlock
		// deferred call.
		if newfile == nil {
			return
		}
		if cerr := newfile.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()

	// Check for duplicate filename inside the lock to avoid race conditions
	exists, err := afs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
	if err != nil {
		return err
	}
	if exists {
		return os.ErrExist
	}

	newsize, maxsize, capsize, err := vfs.CheckAvailableDiskSpace(afs, olddoc)
	if err != nil {
		return err
	}

	f, err := afero.TempFile(afs.fs, "/", newdoc.DocName)
	if err != nil {
		return err
	}
	tmppath := path.Join("/", f.Name())

	// XXX: we use the internal openFile method as we already have a VFS lock
	content, err := afs.openFile(olddoc)
	if err != nil {
		return err
	}
	defer content.Close()

	hash := md5.New()

	newfile = &aferoFileCreation{
		afs:     afs,
		f:       f,
		newdoc:  newdoc,
		tmppath: tmppath,
		w:       0,
		size:    newsize,
		maxsize: maxsize,
		capsize: capsize,
		hash:    hash,
	}

	_, err = io.Copy(newfile, content)
	return err
}

func (afs *aferoVFS) DissociateFile(src, dst *vfs.FileDoc) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()

	// Move the source file to the destination
	needRename := true
	from, err := afs.Indexer.FilePath(src)
	if errors.Is(err, vfs.ErrParentDoesNotExist) {
		needRename = false // The parent directory has already been dissociated
	} else if err != nil {
		return err
	}
	to, err := afs.Indexer.FilePath(dst)
	if err != nil {
		return err
	}
	if from == to {
		needRename = false
	}

	if needRename {
		if err = safeRenameFile(afs.fs, from, to); err != nil {
			return err
		}
	}
	if err = afs.Indexer.CreateFileDoc(dst); err != nil {
		if needRename {
			_ = afs.fs.Rename(to, from)
		}
		return err
	}

	// Clean the source file and its versions
	if err = afs.Indexer.DeleteFileDoc(src); err != nil {
		return err
	}
	_ = afs.fs.RemoveAll(pathForVersions(src.DocID))
	versions, err := vfs.VersionsFor(afs, src.DocID)
	if err != nil {
		return nil
	}
	if from != to {
		_ = afs.Indexer.BatchDeleteVersions(versions)
	}
	return nil
}

func (afs *aferoVFS) DissociateDir(src, dst *vfs.DirDoc) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()

	from := src.Fullpath
	to := dst.Fullpath
	needRename := from != to
	if needRename {
		if _, err := afs.fs.Stat(from); os.IsNotExist(err) {
			needRename = false // The parent directory of the dir was moved
		}
	}
	if needRename {
		if err := safeRenameDir(afs, from, to); err != nil {
			return err
		}
	}
	if err := afs.Indexer.CreateDirDoc(dst); err != nil {
		if needRename {
			_ = afs.fs.Rename(to, from)
		}
		return err
	}
	return afs.Indexer.DeleteDirDoc(src)
}

func (afs *aferoVFS) DestroyDirContent(doc *vfs.DirDoc, push func(vfs.TrashJournal) error) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	diskUsage, _ := afs.DiskUsage()
	files, destroyed, err := afs.Indexer.DeleteDirDocAndContent(doc, true)
	if err != nil {
		return err
	}
	vfs.DiskQuotaAfterDestroy(afs, diskUsage, destroyed)
	infos, err := afero.ReadDir(afs.fs, doc.Fullpath)
	if err != nil {
		return err
	}
	for _, info := range infos {
		fullpath := path.Join(doc.Fullpath, info.Name())
		if info.IsDir() {
			err = afs.fs.RemoveAll(fullpath)
		} else {
			err = afs.fs.Remove(fullpath)
		}
		if err != nil {
			return err
		}
	}
	var allVersions []*vfs.Version
	for _, file := range files {
		_ = afs.fs.RemoveAll(pathForVersions(file.DocID))
		if versions, err := vfs.VersionsFor(afs, file.DocID); err == nil {
			allVersions = append(allVersions, versions...)
		}
	}
	return afs.Indexer.BatchDeleteVersions(allVersions)
}

func (afs *aferoVFS) DestroyDirAndContent(doc *vfs.DirDoc, push func(vfs.TrashJournal) error) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	diskUsage, _ := afs.DiskUsage()
	files, destroyed, err := afs.Indexer.DeleteDirDocAndContent(doc, false)
	if err != nil {
		return err
	}
	vfs.DiskQuotaAfterDestroy(afs, diskUsage, destroyed)
	if err = afs.fs.RemoveAll(doc.Fullpath); err != nil {
		return err
	}
	var allVersions []*vfs.Version
	for _, file := range files {
		_ = afs.fs.RemoveAll(pathForVersions(file.DocID))
		if versions, err := vfs.VersionsFor(afs, file.DocID); err == nil {
			allVersions = append(allVersions, versions...)
		}
	}
	return afs.Indexer.BatchDeleteVersions(allVersions)
}

func (afs *aferoVFS) DestroyFile(doc *vfs.FileDoc) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	diskUsage, _ := afs.DiskUsage()
	name, err := afs.Indexer.FilePath(doc)
	if err != nil {
		return err
	}
	vfs.DiskQuotaAfterDestroy(afs, diskUsage, doc.ByteSize)
	err = afs.fs.Remove(name)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = afs.Indexer.DestroyFileDoc(doc); err != nil {
		return err
	}
	versions, err := vfs.VersionsFor(afs, doc.DocID)
	if err != nil {
		return err
	}
	_ = afs.fs.RemoveAll(pathForVersions(doc.DocID))
	return afs.Indexer.BatchDeleteVersions(versions)
}

func (afs *aferoVFS) openFile(doc *vfs.FileDoc) (vfs.File, error) {
	name, err := afs.Indexer.FilePath(doc)
	if err != nil {
		return nil, err
	}
	f, err := afs.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return &aferoFileOpen{f}, nil
}

func (afs *aferoVFS) OpenFile(doc *vfs.FileDoc) (vfs.File, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer afs.mu.RUnlock()
	return afs.openFile(doc)
}

func (afs *aferoVFS) EnsureErased(journal vfs.TrashJournal) error {
	return errors.New("EnsureErased is only for Swift")
}

func (afs *aferoVFS) OpenFileVersion(doc *vfs.FileDoc, version *vfs.Version) (vfs.File, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer afs.mu.RUnlock()
	f, err := afs.fs.Open(pathForVersion(version))
	if err != nil {
		return nil, err
	}
	return &aferoFileOpen{f}, nil
}

func (afs *aferoVFS) ImportFileVersion(version *vfs.Version, content io.ReadCloser) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()

	diskQuota := afs.DiskQuota()
	if diskQuota > 0 {
		diskUsage, err := afs.DiskUsage()
		if err != nil {
			return err
		}
		if diskUsage+version.ByteSize > diskQuota {
			return vfs.ErrFileTooBig
		}
	}

	vPath := pathForVersion(version)
	_ = afs.fs.MkdirAll(filepath.Dir(vPath), 0755)
	err := afero.WriteReader(afs.fs, vPath, content)
	if errc := content.Close(); err == nil {
		err = errc
	}
	if err != nil {
		// remove the temporary file if an error occurred
		_ = afs.fs.Remove(vPath)
		return err
	}

	return afs.Indexer.CreateVersion(version)
}

func (afs *aferoVFS) RevertFileVersion(doc *vfs.FileDoc, version *vfs.Version) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()

	mainpath, err := afs.Indexer.FilePath(doc)
	if err != nil {
		return err
	}

	save := vfs.NewVersion(doc)
	savepath := pathForVersion(save)
	frompath := pathForVersion(version)

	if err = afs.fs.Rename(mainpath, savepath); err != nil {
		return err
	}

	if err = afs.fs.Rename(frompath, mainpath); err != nil {
		_ = afs.fs.Rename(savepath, mainpath)
		return err
	}

	newdoc := doc.Clone().(*vfs.FileDoc)
	vfs.SetMetaFromVersion(newdoc, version)
	if err = afs.Indexer.UpdateFileDoc(doc, newdoc); err != nil {
		_ = afs.fs.Rename(mainpath, frompath)
		_ = afs.fs.Rename(savepath, mainpath)
		return err
	}

	_ = afs.Indexer.DeleteVersion(version)

	if err = afs.Indexer.CreateVersion(save); err != nil {
		_ = afs.fs.Remove(savepath)
	}

	return nil
}

func (afs *aferoVFS) CopyFileFromOtherFS(
	newdoc, olddoc *vfs.FileDoc,
	srcFS vfs.Fs,
	srcDoc *vfs.FileDoc,
) error {
	content, err := srcFS.OpenFile(srcDoc)
	if err != nil {
		return err
	}
	defer content.Close()

	fd, err := afs.CreateFile(newdoc, olddoc)
	if err != nil {
		return err
	}

	_, err = io.Copy(fd, content)
	errc := fd.Close()
	if err != nil {
		return err
	}
	return errc
}

// UpdateFileDoc overrides the indexer's one since the afero.Fs is by essence
// also indexed by path. When moving a file, the index has to be moved and the
// filesystem should also be updated.
//
// @override Indexer.UpdateFileDoc
func (afs *aferoVFS) UpdateFileDoc(olddoc, newdoc *vfs.FileDoc) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	if newdoc.DirID != olddoc.DirID || newdoc.DocName != olddoc.DocName {
		oldpath, err := afs.Indexer.FilePath(olddoc)
		if err != nil {
			return err
		}
		newpath, err := afs.Indexer.FilePath(newdoc)
		if err != nil {
			return err
		}
		err = safeRenameFile(afs.fs, oldpath, newpath)
		if err != nil {
			return err
		}
	}
	if newdoc.Executable != olddoc.Executable {
		newpath, err := afs.Indexer.FilePath(newdoc)
		if err != nil {
			return err
		}
		err = afs.fs.Chmod(newpath, newdoc.Mode())
		if err != nil {
			return err
		}
	}
	return afs.Indexer.UpdateFileDoc(olddoc, newdoc)
}

// UpdateDirDoc overrides the indexer's one since the afero.Fs is by essence
// also indexed by path. When moving a file, the index has to be moved and the
// filesystem should also be updated.
//
// @override Indexer.UpdateDirDoc
func (afs *aferoVFS) UpdateDirDoc(olddoc, newdoc *vfs.DirDoc) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	if newdoc.Fullpath != olddoc.Fullpath {
		if err := safeRenameDir(afs, olddoc.Fullpath, newdoc.Fullpath); err != nil {
			return err
		}
	}
	return afs.Indexer.UpdateDirDoc(olddoc, newdoc)
}

func (afs *aferoVFS) DirByID(fileID string) (*vfs.DirDoc, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer afs.mu.RUnlock()
	return afs.Indexer.DirByID(fileID)
}

func (afs *aferoVFS) DirByPath(name string) (*vfs.DirDoc, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer afs.mu.RUnlock()
	return afs.Indexer.DirByPath(name)
}

func (afs *aferoVFS) FileByID(fileID string) (*vfs.FileDoc, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer afs.mu.RUnlock()
	return afs.Indexer.FileByID(fileID)
}

func (afs *aferoVFS) FileByPath(name string) (*vfs.FileDoc, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer afs.mu.RUnlock()
	return afs.Indexer.FileByPath(name)
}

func (afs *aferoVFS) FilePath(doc *vfs.FileDoc) (string, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return "", lockerr
	}
	defer afs.mu.RUnlock()
	return afs.Indexer.FilePath(doc)
}

func (afs *aferoVFS) DirOrFileByID(fileID string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return nil, nil, lockerr
	}
	defer afs.mu.RUnlock()
	return afs.Indexer.DirOrFileByID(fileID)
}

func (afs *aferoVFS) DirOrFileByPath(name string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	if lockerr := afs.mu.RLock(); lockerr != nil {
		return nil, nil, lockerr
	}
	defer afs.mu.RUnlock()
	return afs.Indexer.DirOrFileByPath(name)
}

// aferoFileOpen represents a file handle opened for reading.
type aferoFileOpen struct {
	f afero.File
}

func (f *aferoFileOpen) Read(p []byte) (int, error) {
	return f.f.Read(p)
}

func (f *aferoFileOpen) ReadAt(p []byte, off int64) (int, error) {
	return f.f.ReadAt(p, off)
}

func (f *aferoFileOpen) Seek(offset int64, whence int) (int64, error) {
	return f.f.Seek(offset, whence)
}

func (f *aferoFileOpen) Write(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f *aferoFileOpen) Close() error {
	return f.f.Close()
}

// aferoFileCreation represents a file open for writing. It is used to create a
// file or to modify the content of a file.
//
// aferoFileCreation implements io.WriteCloser.
type aferoFileCreation struct {
	afs     *aferoVFS          // parent vfs
	f       afero.File         // file handle
	newdoc  *vfs.FileDoc       // new document
	olddoc  *vfs.FileDoc       // old document
	tmppath string             // temporary file path for uploading a new version of this file
	w       int64              // total size written
	size    int64              // total file size, -1 if unknown
	maxsize int64              // maximum size allowed for the file
	capsize int64              // size cap from which we send a notification to the user
	hash    hash.Hash          // hash we build up along the file
	meta    *vfs.MetaExtractor // extracts metadata from the content
	err     error              // write error
}

func (f *aferoFileCreation) Read(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f *aferoFileCreation) ReadAt(p []byte, off int64) (int, error) {
	return 0, os.ErrInvalid
}

func (f *aferoFileCreation) Seek(offset int64, whence int) (int64, error) {
	return 0, os.ErrInvalid
}

func (f *aferoFileCreation) Write(p []byte) (int, error) {
	if f.meta != nil {
		if _, err := (*f.meta).Write(p); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			(*f.meta).Abort(err)
			f.meta = nil
		}
	}

	n, err := f.f.Write(p)
	if err != nil {
		f.err = err
		return n, err
	}

	f.w += int64(n)
	if f.maxsize >= 0 && f.w > f.maxsize {
		f.err = vfs.ErrFileTooBig
		return n, f.err
	}

	if f.size >= 0 && f.w > f.size {
		f.err = vfs.ErrContentLengthMismatch
		return n, f.err
	}

	_, err = f.hash.Write(p)
	return n, err
}

func (f *aferoFileCreation) Close() (err error) {
	defer func() {
		if err != nil {
			// Remove the temporary file if an error occurred
			_ = f.afs.fs.Remove(f.tmppath)
			// If an error has occurred when creating a new file, we should
			// also delete the file from the index.
			if f.olddoc == nil {
				_ = f.afs.Indexer.DeleteFileDoc(f.newdoc)
			}
		}
	}()

	if err = f.f.Close(); err != nil {
		if f.meta != nil {
			(*f.meta).Abort(err)
		}
		if f.err == nil {
			f.err = err
		}
	}

	newdoc, olddoc, written := f.newdoc, f.olddoc, f.w

	if f.meta != nil {
		if errc := (*f.meta).Close(); errc == nil {
			vfs.MergeMetadata(newdoc, (*f.meta).Result())
		}
	}

	if f.err != nil {
		return f.err
	}

	md5sum := f.hash.Sum(nil)
	if newdoc.MD5Sum == nil {
		newdoc.MD5Sum = md5sum
	}

	if !bytes.Equal(newdoc.MD5Sum, md5sum) {
		return vfs.ErrInvalidHash
	}

	if f.size < 0 {
		newdoc.ByteSize = written
	}

	if newdoc.ByteSize != written {
		return vfs.ErrContentLengthMismatch
	}

	lockerr := f.afs.mu.Lock()
	if lockerr != nil {
		return lockerr
	}
	defer f.afs.mu.Unlock()

	// Check again that a file with the same path does not exist. It can happen
	// when the same file is uploaded twice in parallel.
	if olddoc == nil {
		exists, err := f.afs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return err
		}
		if exists {
			return os.ErrExist
		}
	}

	var newpath string
	newpath, err = f.afs.Indexer.FilePath(newdoc)
	if err != nil {
		return err
	}
	if strings.HasPrefix(newpath, vfs.TrashDirName+"/") {
		return vfs.ErrParentInTrash
	}

	var v *vfs.Version
	if olddoc != nil {
		v = vfs.NewVersion(olddoc)
		err = f.afs.Indexer.UpdateFileDoc(olddoc, newdoc)
	} else if newdoc.ID() == "" {
		err = f.afs.Indexer.CreateFileDoc(newdoc)
	} else {
		err = f.afs.Indexer.CreateNamedFileDoc(newdoc)
	}
	if err != nil {
		return err
	}

	if v != nil {
		vPath := pathForVersion(v)
		_ = f.afs.fs.MkdirAll(filepath.Dir(vPath), 0755)
		if err = f.afs.fs.Rename(newpath, vPath); err != nil {
			// If we can't move the content, we just don't create the version,
			// but still let the upload for the new content finishes.
			v = nil
		}
	}

	// move the temporary file to its final location
	if err = f.afs.fs.Rename(f.tmppath, newpath); err != nil {
		if v != nil {
			vPath := pathForVersion(v)
			_ = f.afs.fs.Rename(vPath, newpath)
		}
		return err
	}

	if v != nil {
		actionV, toClean, _ := vfs.FindVersionsToClean(f.afs, newdoc.DocID, v)
		if bytes.Equal(newdoc.MD5Sum, olddoc.MD5Sum) {
			actionV = vfs.CleanCandidateVersion
		}
		if actionV == vfs.KeepCandidateVersion {
			if errv := f.afs.Indexer.CreateVersion(v); errv != nil {
				actionV = vfs.CleanCandidateVersion
			}
		}
		if actionV == vfs.CleanCandidateVersion {
			vPath := pathForVersion(v)
			_ = f.afs.fs.Remove(vPath)
		}
		for _, old := range toClean {
			_ = cleanOldVersion(f.afs, old)
		}
	}

	if f.capsize > 0 && f.size >= f.capsize {
		vfs.PushDiskQuotaAlert(f.afs, true)
	}

	return nil
}

func safeRenameFile(fs afero.Fs, oldpath, newpath string) error {
	newpath = path.Clean(newpath)
	oldpath = path.Clean(oldpath)

	if !path.IsAbs(newpath) || !path.IsAbs(oldpath) {
		return vfs.ErrNonAbsolutePath
	}

	_, err := fs.Stat(newpath)
	if err == nil {
		return os.ErrExist
	}
	if !os.IsNotExist(err) {
		return err
	}

	return fs.Rename(oldpath, newpath)
}

func safeRenameDir(afs *aferoVFS, oldpath, newpath string) error {
	newpath = path.Clean(newpath)
	oldpath = path.Clean(oldpath)

	if !path.IsAbs(newpath) || !path.IsAbs(oldpath) {
		return vfs.ErrNonAbsolutePath
	}

	if strings.HasPrefix(newpath, oldpath+"/") {
		return vfs.ErrForbiddenDocMove
	}

	_, err := afs.fs.Stat(newpath)
	if err == nil {
		return os.ErrExist
	}
	if !os.IsNotExist(err) {
		return err
	}

	return afs.fs.Rename(oldpath, newpath)
}

func extractContentTypeAndMD5(filename string) (contentType string, md5sum []byte, err error) {
	f, err := os.Open(filename)
	if err != nil {
		return
	}
	defer f.Close()
	var r io.Reader
	contentType, r = filetype.FromReader(f)
	h := md5.New()
	if _, err = io.Copy(h, r); err != nil {
		return
	}
	md5sum = h.Sum(nil)
	return
}

func (afs *aferoVFS) CleanOldVersion(fileID string, version *vfs.Version) error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	return cleanOldVersion(afs, version)
}

func cleanOldVersion(afs *aferoVFS, version *vfs.Version) error {
	if err := afs.Indexer.DeleteVersion(version); err != nil {
		return err
	}
	vPath := pathForVersion(version)
	return afs.fs.Remove(vPath)
}

func pathForVersion(v *vfs.Version) string {
	parts := strings.SplitN(v.DocID, "/", 2)
	fileID := parts[0]
	versionID := parts[0]
	if len(parts) > 1 {
		versionID = parts[1]
	}
	return path.Join(pathForVersions(fileID), versionID)
}

func pathForVersions(fileID string) string {
	// Avoid too many files in the same directory by using some sub-directories
	return path.Join(vfs.VersionsDirName, fileID[:4], fileID[4:])
}

func (afs *aferoVFS) ClearOldVersions() error {
	if lockerr := afs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer afs.mu.Unlock()
	versions, err := afs.Indexer.AllVersions()
	if err != nil {
		return err
	}
	if err := afs.Indexer.BatchDeleteVersions(versions); err != nil {
		return err
	}
	return afs.fs.RemoveAll(vfs.VersionsDirName)
}

var (
	_ vfs.VFS  = &aferoVFS{}
	_ vfs.File = &aferoFileOpen{}
	_ vfs.File = &aferoFileCreation{}
)
