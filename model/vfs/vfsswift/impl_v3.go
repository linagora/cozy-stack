// Package vfsswift is the implementation of the Virtual File System by using
// Swift from the OpenStack project. The file contents are saved in the object
// storage (Swift), and the metadata are indexed in CouchDB.
package vfsswift

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/gofrs/uuid/v5"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/ncw/swift/v2"
)

type swiftVFSV3 struct {
	vfs.Indexer
	vfs.DiskThresholder
	c         *swift.Connection
	cluster   int
	domain    string
	prefix    string
	context   string
	container string
	mu        lock.ErrorRWLocker
	ctx       context.Context
	log       *logger.Entry
}

const swiftV3ContainerPrefix = "cozy-v3-"
const maxFileSize = 5 << (3 * 10) // 5 GiB

// NewV3 returns a vfs.VFS instance associated with the specified indexer and
// the swift storage url.
//
// This new V3 version uses only a single swift container per instance. We can
// easily put the thumbnails in the same container that the data. And, for the
// versioning, Swift Object Versioning is not what we want: it is not as robust
// as we expected (we had encoding issue with the V1 layout for file with `? `
// in the name), and it is poor in features (for example, we want to swap an
// old version with the current version without having to download/upload
// contents, and it is not supported).
func NewV3(db vfs.Prefixer, index vfs.Indexer, disk vfs.DiskThresholder, mu lock.ErrorRWLocker) (vfs.VFS, error) {
	return &swiftVFSV3{
		Indexer:         index,
		DiskThresholder: disk,

		c:         config.GetSwiftConnection(),
		cluster:   db.DBCluster(),
		domain:    db.DomainName(),
		prefix:    db.DBPrefix(),
		context:   db.GetContextName(),
		container: swiftV3ContainerPrefix + db.DBPrefix(),
		mu:        mu,
		ctx:       context.Background(),
		log:       logger.WithDomain(db.DomainName()).WithNamespace("vfsswift"),
	}, nil
}

// NewInternalID returns a random string that can be used as an internal_vfs_id.
func NewInternalID() string {
	return utils.RandomString(16)
}

// MakeObjectNameV3 builds the swift object name for a given file document. It
// creates a virtual subfolder by splitting the document ID, which should be 32
// bytes long, on the 27nth byte. This avoid having a flat hierarchy in swift
// with no bound. And it appends the internalID at the end to regroup all the
// versions of a file in the same virtual subfolder.
func MakeObjectNameV3(docID, internalID string) string {
	if len(docID) != 32 || len(internalID) != 16 {
		return docID + "/" + internalID
	}
	return docID[:22] + "/" + docID[22:27] + "/" + docID[27:] + "/" + internalID
}

func makeDocIDV3(objName string) (string, string) {
	if len(objName) != 51 {
		parts := strings.SplitN(objName, "/", 2)
		if len(parts) < 2 {
			return objName, ""
		}
		return parts[0], parts[1]
	}
	return objName[:22] + objName[23:28] + objName[29:34], objName[35:]
}

func (sfs *swiftVFSV3) MaxFileSize() int64 {
	return maxFileSize
}

func (sfs *swiftVFSV3) DBCluster() int {
	return sfs.cluster
}

func (sfs *swiftVFSV3) DBPrefix() string {
	return sfs.prefix
}

func (sfs *swiftVFSV3) DomainName() string {
	return sfs.domain
}

func (sfs *swiftVFSV3) GetContextName() string {
	return sfs.context
}

func (sfs *swiftVFSV3) GetIndexer() vfs.Indexer {
	return sfs.Indexer
}

func (sfs *swiftVFSV3) UseSharingIndexer(index vfs.Indexer) vfs.VFS {
	return &swiftVFSV3{
		Indexer:         index,
		DiskThresholder: sfs.DiskThresholder,
		c:               sfs.c,
		domain:          sfs.domain,
		prefix:          sfs.prefix,
		container:       sfs.container,
		mu:              sfs.mu,
		ctx:             context.Background(),
		log:             sfs.log,
	}
}

func (sfs *swiftVFSV3) ContainerNames() map[string]string {
	return map[string]string{"container": sfs.container}
}

func (sfs *swiftVFSV3) InitFs() error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	if err := sfs.Indexer.InitIndex(); err != nil {
		return err
	}
	if err := sfs.c.ContainerCreate(sfs.ctx, sfs.container, nil); err != nil {
		sfs.log.Errorf("Could not create container %q: %s",
			sfs.container, err.Error())
		return err
	}
	sfs.log.Infof("Created container %q", sfs.container)
	return nil
}

func (sfs *swiftVFSV3) Delete() error {
	containerMeta := swift.Metadata{"to-be-deleted": "1"}.ContainerHeaders()
	sfs.log.Infof("Marking container %q as to-be-deleted", sfs.container)
	err := sfs.c.ContainerUpdate(sfs.ctx, sfs.container, containerMeta)
	if err != nil {
		sfs.log.Errorf("Could not mark container %q as to-be-deleted: %s",
			sfs.container, err)
	}
	return DeleteContainer(sfs.ctx, sfs.c, sfs.container)
}

func (sfs *swiftVFSV3) CreateDir(doc *vfs.DirDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	exists, err := sfs.Indexer.DirChildExists(doc.DirID, doc.DocName)
	if err != nil {
		return err
	}
	if exists {
		return os.ErrExist
	}
	if doc.ID() == "" {
		return sfs.Indexer.CreateDirDoc(doc)
	}
	return sfs.Indexer.CreateNamedDirDoc(doc)
}

func (sfs *swiftVFSV3) CreateFile(newdoc, olddoc *vfs.FileDoc, opts ...vfs.CreateOptions) (vfs.File, error) {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.Unlock()

	newsize, maxsize, capsize, err := vfs.CheckAvailableDiskSpace(sfs, newdoc)
	if err != nil {
		return nil, err
	}
	if newsize > maxsize {
		return nil, vfs.ErrFileTooBig
	}

	if olddoc != nil {
		newdoc.SetID(olddoc.ID())
		newdoc.SetRev(olddoc.Rev())
		newdoc.CreatedAt = olddoc.CreatedAt
	}

	newpath, err := sfs.Indexer.FilePath(newdoc)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(newpath, vfs.TrashDirName+"/") {
		if !vfs.OptionsAllowCreationInTrash(opts) {
			return nil, vfs.ErrParentInTrash
		}
	}

	if olddoc == nil {
		var exists bool
		exists, err = sfs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, os.ErrExist
		}
	}

	if newdoc.DocID == "" {
		uid, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		newdoc.DocID = uid.String()
	}

	newdoc.InternalID = NewInternalID()
	objName := MakeObjectNameV3(newdoc.DocID, newdoc.InternalID)
	hash := hex.EncodeToString(newdoc.MD5Sum)
	f, err := sfs.c.ObjectCreate(sfs.ctx, sfs.container, objName, true, hash, newdoc.Mime, nil)
	if err != nil {
		return nil, err
	}
	extractor := vfs.NewMetaExtractor(newdoc)

	return &swiftFileCreationV3{
		fs:      sfs,
		f:       f,
		newdoc:  newdoc,
		olddoc:  olddoc,
		name:    objName,
		w:       0,
		size:    newsize,
		maxsize: maxsize,
		capsize: capsize,
		meta:    extractor,
	}, nil
}

func (sfs *swiftVFSV3) CopyFile(olddoc, newdoc *vfs.FileDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()

	// Check for duplicate filename inside the lock to avoid race conditions
	exists, err := sfs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
	if err != nil {
		return err
	}
	if exists {
		return os.ErrExist
	}

	newsize, _, capsize, err := vfs.CheckAvailableDiskSpace(sfs, olddoc)
	if err != nil {
		return err
	}

	uid, err := uuid.NewV7()
	if err != nil {
		return err
	}
	newdoc.DocID = uid.String()
	newdoc.InternalID = NewInternalID()

	// Copy the file
	srcName := MakeObjectNameV3(olddoc.DocID, olddoc.InternalID)
	dstName := MakeObjectNameV3(newdoc.DocID, newdoc.InternalID)
	headers := swift.Metadata{
		"creation-name": newdoc.Name(),
		"created-at":    newdoc.CreatedAt.Format(time.RFC3339),
		"copied-from":   olddoc.ID(),
	}.ObjectHeaders()
	if _, err := sfs.c.ObjectCopy(sfs.ctx, sfs.container, srcName, sfs.container, dstName, headers); err != nil {
		return err
	}
	if err := sfs.Indexer.CreateNamedFileDoc(newdoc); err != nil {
		_ = sfs.c.ObjectDelete(sfs.ctx, sfs.container, dstName)
		return err
	}

	if capsize > 0 && newsize >= capsize {
		vfs.PushDiskQuotaAlert(sfs, true)
	}

	return nil
}

func (sfs *swiftVFSV3) DissociateFile(src, dst *vfs.FileDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()

	if src.DirID != dst.DirID || src.DocName != dst.DocName {
		exists, err := sfs.Indexer.DirChildExists(dst.DirID, dst.DocName)
		if err != nil {
			return err
		}
		if exists {
			return os.ErrExist
		}
	}

	uid, err := uuid.NewV7()
	if err != nil {
		return err
	}
	dst.DocID = uid.String()

	// Copy the file
	srcName := MakeObjectNameV3(src.DocID, src.InternalID)
	dstName := MakeObjectNameV3(dst.DocID, dst.InternalID)
	headers := swift.Metadata{
		"creation-name":  src.Name(),
		"created-at":     src.CreatedAt.Format(time.RFC3339),
		"dissociated-of": src.ID(),
	}.ObjectHeaders()
	if _, err := sfs.c.ObjectCopy(sfs.ctx, sfs.container, srcName, sfs.container, dstName, headers); err != nil {
		return err
	}
	if err := sfs.Indexer.CreateNamedFileDoc(dst); err != nil {
		_ = sfs.c.ObjectDelete(sfs.ctx, sfs.container, dstName)
		return err
	}

	// Remove the source
	thumbsFS := &thumbsV3{
		c:         sfs.c,
		container: sfs.container,
		ctx:       context.Background(),
	}
	if err := thumbsFS.RemoveThumbs(src, vfs.ThumbnailFormatNames); err != nil {
		sfs.log.Infof("Cleaning thumbnails in DissociateFile %s has failed: %s", src.ID(), err)
	}
	return sfs.destroyFileLocked(src)
}

func (sfs *swiftVFSV3) DissociateDir(src, dst *vfs.DirDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()

	if dst.DirID != src.DirID || dst.DocName != src.DocName {
		exists, err := sfs.Indexer.DirChildExists(dst.DirID, dst.DocName)
		if err != nil {
			return err
		}
		if exists {
			return os.ErrExist
		}
	}

	if err := sfs.Indexer.CreateDirDoc(dst); err != nil {
		return err
	}
	return sfs.Indexer.DeleteDirDoc(src)
}

func (sfs *swiftVFSV3) destroyDir(doc *vfs.DirDoc, push func(vfs.TrashJournal) error, onlyContent bool) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	diskUsage, _ := sfs.Indexer.DiskUsage()
	files, destroyed, err := sfs.Indexer.DeleteDirDocAndContent(doc, onlyContent)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	vfs.DiskQuotaAfterDestroy(sfs, diskUsage, destroyed)
	ids := make([]string, len(files))
	objNames := make([]string, len(files))
	for i, file := range files {
		ids[i] = file.DocID
		objNames[i] = MakeObjectNameV3(file.DocID, file.InternalID)
	}
	err = push(vfs.TrashJournal{
		FileIDs:     ids,
		ObjectNames: objNames,
	})
	return err
}

func (sfs *swiftVFSV3) DestroyDirContent(doc *vfs.DirDoc, push func(vfs.TrashJournal) error) error {
	return sfs.destroyDir(doc, push, true)
}

func (sfs *swiftVFSV3) DestroyDirAndContent(doc *vfs.DirDoc, push func(vfs.TrashJournal) error) error {
	return sfs.destroyDir(doc, push, false)
}

func (sfs *swiftVFSV3) DestroyFile(doc *vfs.FileDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	return sfs.destroyFileLocked(doc)
}

func (sfs *swiftVFSV3) destroyFileLocked(doc *vfs.FileDoc) error {
	diskUsage, _ := sfs.Indexer.DiskUsage()
	objNames := []string{
		MakeObjectNameV3(doc.DocID, doc.InternalID),
	}
	if err := sfs.Indexer.DeleteFileDoc(doc); err != nil {
		return err
	}
	destroyed := doc.ByteSize
	if versions, errv := vfs.VersionsFor(sfs, doc.DocID); errv == nil {
		for _, v := range versions {
			internalID := v.DocID
			if parts := strings.SplitN(v.DocID, "/", 2); len(parts) > 1 {
				internalID = parts[1]
			}
			objNames = append(objNames, MakeObjectNameV3(doc.DocID, internalID))
			destroyed += v.ByteSize
		}
		err := sfs.Indexer.BatchDeleteVersions(versions)
		if err != nil {
			sfs.log.Warnf("DestroyFile failed on BatchDeleteVersions: %s", err)
		}
	}
	_, errb := sfs.c.BulkDelete(sfs.ctx, sfs.container, objNames)
	if errb == swift.Forbidden {
		for _, objName := range objNames {
			if err := sfs.c.ObjectDelete(sfs.ctx, sfs.container, objName); err != nil {
				sfs.log.Infof("DestroyFile failed on ObjectDelete: %s", err)
			}
		}
	}
	if errb != nil {
		sfs.log.Warnf("DestroyFile failed on BulkDelete: %s", errb)
	}
	vfs.DiskQuotaAfterDestroy(sfs, diskUsage, destroyed)
	return nil
}

func (sfs *swiftVFSV3) EnsureErased(journal vfs.TrashJournal) error {
	// No lock needed
	diskUsage, _ := sfs.Indexer.DiskUsage()
	objNames := journal.ObjectNames
	var errm error
	var destroyed int64
	var allVersions []*vfs.Version
	for _, fileID := range journal.FileIDs {
		versions, err := vfs.VersionsFor(sfs, fileID)
		if err != nil {
			if !couchdb.IsNoDatabaseError(err) {
				sfs.log.Warnf("EnsureErased failed on VersionsFor(%s): %s", fileID, err)
				errm = multierror.Append(errm, err)
			}
			continue
		}
		for _, v := range versions {
			internalID := v.DocID
			if parts := strings.SplitN(v.DocID, "/", 2); len(parts) > 1 {
				internalID = parts[1]
			}
			objNames = append(objNames, MakeObjectNameV3(fileID, internalID))
			destroyed += v.ByteSize
		}
		allVersions = append(allVersions, versions...)
	}
	if err := sfs.Indexer.BatchDeleteVersions(allVersions); err != nil {
		sfs.log.Warnf("EnsureErased failed on BatchDeleteVersions: %s", err)
		errm = multierror.Append(errm, err)
	}
	if err := deleteContainerFiles(sfs.ctx, sfs.c, sfs.container, objNames); err != nil {
		sfs.log.Warnf("EnsureErased failed on deleteContainerFiles: %s", err)
		errm = multierror.Append(errm, err)
	}
	vfs.DiskQuotaAfterDestroy(sfs, diskUsage, destroyed)
	return errm
}

func (sfs *swiftVFSV3) OpenFile(doc *vfs.FileDoc) (vfs.File, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	objName := MakeObjectNameV3(doc.DocID, doc.InternalID)
	f, _, err := sfs.c.ObjectOpen(sfs.ctx, sfs.container, objName, false, nil)
	if errors.Is(err, swift.ObjectNotFound) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return &swiftFileOpenV3{f, nil}, nil
}

func (sfs *swiftVFSV3) OpenFileVersion(doc *vfs.FileDoc, version *vfs.Version) (vfs.File, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	internalID := version.DocID
	if parts := strings.SplitN(version.DocID, "/", 2); len(parts) > 1 {
		internalID = parts[1]
	}
	objName := MakeObjectNameV3(doc.DocID, internalID)
	f, _, err := sfs.c.ObjectOpen(sfs.ctx, sfs.container, objName, false, nil)
	if errors.Is(err, swift.ObjectNotFound) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return &swiftFileOpenV3{f, nil}, nil
}

func (sfs *swiftVFSV3) ImportFileVersion(version *vfs.Version, content io.ReadCloser) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()

	diskQuota := sfs.DiskQuota()
	if diskQuota > 0 {
		diskUsage, err := sfs.DiskUsage()
		if err != nil {
			return err
		}
		if diskUsage+version.ByteSize > diskQuota {
			return vfs.ErrFileTooBig
		}
	}

	parts := strings.SplitN(version.DocID, "/", 2)
	if len(parts) != 2 {
		return vfs.ErrIllegalFilename
	}
	objName := MakeObjectNameV3(parts[0], parts[1])

	hash := hex.EncodeToString(version.MD5Sum)
	f, err := sfs.c.ObjectCreate(sfs.ctx, sfs.container, objName, true, hash, "application/octet-stream", nil)
	if err != nil {
		return err
	}

	_, err = io.Copy(f, content)
	if errc := content.Close(); err == nil {
		err = errc
	}
	if errc := f.Close(); err == nil {
		err = errc
	}
	if err != nil {
		if errors.Is(err, swift.ObjectCorrupted) {
			err = vfs.ErrInvalidHash
		}
		return err
	}

	return sfs.Indexer.CreateVersion(version)
}

// WriteContentAt streams content into the object backing the (docID,
// internalID) key in this instance's container, creating NO CouchDB
// document. Used by storage migration, which preserves the shared index and
// only moves bytes.
func (sfs *swiftVFSV3) WriteContentAt(docID, internalID string, content io.Reader, size int64) error {
	objName := MakeObjectNameV3(docID, internalID)
	f, err := sfs.c.ObjectCreate(sfs.ctx, sfs.container, objName, true, "", "application/octet-stream", nil)
	if err != nil {
		return err
	}
	if _, err = io.Copy(f, content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// StatContentAt returns the byte size of the object backing the (docID,
// internalID) key in this instance's container, without touching CouchDB. It
// returns os.ErrNotExist when the object is absent. Used by storage
// migration to verify a copy landed on the target before flipping the
// instance's backend flag.
func (sfs *swiftVFSV3) StatContentAt(docID, internalID string) (int64, error) {
	objName := MakeObjectNameV3(docID, internalID)
	info, _, err := sfs.c.Object(sfs.ctx, sfs.container, objName)
	if errors.Is(err, swift.ObjectNotFound) {
		return 0, os.ErrNotExist
	}
	if err != nil {
		return 0, err
	}
	return info.Bytes, nil
}

func (sfs *swiftVFSV3) RevertFileVersion(doc *vfs.FileDoc, version *vfs.Version) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()

	save := vfs.NewVersion(doc)
	if err := sfs.Indexer.CreateVersion(save); err != nil {
		return err
	}

	newdoc := doc.Clone().(*vfs.FileDoc)
	if parts := strings.SplitN(version.DocID, "/", 2); len(parts) > 1 {
		newdoc.InternalID = parts[1]
	}
	vfs.SetMetaFromVersion(newdoc, version)
	if err := sfs.Indexer.UpdateFileDoc(doc, newdoc); err != nil {
		_ = sfs.Indexer.DeleteVersion(save)
		return err
	}

	return sfs.Indexer.DeleteVersion(version)
}

func (sfs *swiftVFSV3) CopyFileFromOtherFS(
	newdoc, olddoc *vfs.FileDoc,
	srcFS vfs.Fs,
	srcDoc *vfs.FileDoc,
) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()

	newsize, maxsize, capsize, err := vfs.CheckAvailableDiskSpace(sfs, newdoc)
	if err != nil {
		return err
	}
	if newsize > maxsize {
		return vfs.ErrFileTooBig
	}

	newpath, err := sfs.Indexer.FilePath(newdoc)
	if err != nil {
		return err
	}
	if strings.HasPrefix(newpath, vfs.TrashDirName+"/") {
		return vfs.ErrParentInTrash
	}

	if olddoc == nil {
		var exists bool
		exists, err = sfs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return err
		}
		if exists {
			return os.ErrExist
		}
	}

	if newdoc.DocID == "" {
		uid, err := uuid.NewV7()
		if err != nil {
			return err
		}
		newdoc.DocID = uid.String()
	}

	newdoc.InternalID = NewInternalID()

	srcName := MakeObjectNameV3(srcDoc.DocID, srcDoc.InternalID)
	dstName := MakeObjectNameV3(newdoc.DocID, newdoc.InternalID)
	srcContainer := srcFS.(*swiftVFSV3).container
	if _, err := sfs.c.ObjectCopy(sfs.ctx, srcContainer, srcName, sfs.container, dstName, nil); err != nil {
		return err
	}

	var v *vfs.Version
	if olddoc != nil {
		v = vfs.NewVersion(olddoc)
		err = sfs.Indexer.UpdateFileDoc(olddoc, newdoc)
	} else {
		err = sfs.Indexer.CreateNamedFileDoc(newdoc)
	}
	if err != nil {
		return err
	}

	if v != nil {
		actionV, toClean, _ := vfs.FindVersionsToClean(sfs, newdoc.DocID, v)
		if bytes.Equal(newdoc.MD5Sum, olddoc.MD5Sum) {
			actionV = vfs.CleanCandidateVersion
		}
		if actionV == vfs.KeepCandidateVersion {
			if errv := sfs.Indexer.CreateVersion(v); errv != nil {
				actionV = vfs.CleanCandidateVersion
			}
		}
		if actionV == vfs.CleanCandidateVersion {
			internalID := v.DocID
			if parts := strings.SplitN(v.DocID, "/", 2); len(parts) > 1 {
				internalID = parts[1]
			}
			objName := MakeObjectNameV3(newdoc.DocID, internalID)
			_ = sfs.c.ObjectDelete(sfs.ctx, sfs.container, objName)
		}
		for _, old := range toClean {
			_ = cleanOldVersion(sfs, newdoc.DocID, old)
		}
	}

	if capsize > 0 && newsize >= capsize {
		vfs.PushDiskQuotaAlert(sfs, true)
	}

	return nil
}

// UpdateFileDoc calls the indexer UpdateFileDoc function and adds a few checks
// before actually calling this method:
//   - locks the filesystem for writing
//   - checks in case we have a move operation that the new path is available
//
// @override Indexer.UpdateFileDoc
func (sfs *swiftVFSV3) UpdateFileDoc(olddoc, newdoc *vfs.FileDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	if newdoc.DirID != olddoc.DirID || newdoc.DocName != olddoc.DocName {
		exists, err := sfs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return err
		}
		if exists {
			return os.ErrExist
		}
	}
	return sfs.Indexer.UpdateFileDoc(olddoc, newdoc)
}

// UdpdateDirDoc calls the indexer UdpdateDirDoc function and adds a few checks
// before actually calling this method:
//   - locks the filesystem for writing
//   - checks that we don't move a directory to one of its descendant
//   - checks in case we have a move operation that the new path is available
//
// @override Indexer.UpdateDirDoc
func (sfs *swiftVFSV3) UpdateDirDoc(olddoc, newdoc *vfs.DirDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	if newdoc.DirID != olddoc.DirID || newdoc.DocName != olddoc.DocName {
		if strings.HasPrefix(newdoc.Fullpath, olddoc.Fullpath+"/") {
			return vfs.ErrForbiddenDocMove
		}
		exists, err := sfs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return err
		}
		if exists {
			return os.ErrExist
		}
	}
	return sfs.Indexer.UpdateDirDoc(olddoc, newdoc)
}

func (sfs *swiftVFSV3) DirByID(fileID string) (*vfs.DirDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.DirByID(fileID)
}

func (sfs *swiftVFSV3) DirByPath(name string) (*vfs.DirDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.DirByPath(name)
}

func (sfs *swiftVFSV3) FileByID(fileID string) (*vfs.FileDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.FileByID(fileID)
}

func (sfs *swiftVFSV3) FileByPath(name string) (*vfs.FileDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.FileByPath(name)
}

func (sfs *swiftVFSV3) FilePath(doc *vfs.FileDoc) (string, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return "", lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.FilePath(doc)
}

func (sfs *swiftVFSV3) DirOrFileByID(fileID string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.DirOrFileByID(fileID)
}

func (sfs *swiftVFSV3) DirOrFileByPath(name string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.DirOrFileByPath(name)
}

// swiftFileCreationV3 represents a file open for writing. It is used to create
// a file or to modify the content of a file.
//
// swiftFileCreationV3 implements io.WriteCloser.
type swiftFileCreationV3 struct {
	fs      *swiftVFSV3
	f       *swift.ObjectCreateFile
	newdoc  *vfs.FileDoc
	olddoc  *vfs.FileDoc
	name    string
	w       int64
	size    int64
	maxsize int64
	capsize int64
	meta    *vfs.MetaExtractor
	err     error
}

func (f *swiftFileCreationV3) Read(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f *swiftFileCreationV3) ReadAt(p []byte, off int64) (int, error) {
	return 0, os.ErrInvalid
}

func (f *swiftFileCreationV3) Seek(offset int64, whence int) (int64, error) {
	return 0, os.ErrInvalid
}

func (f *swiftFileCreationV3) Write(p []byte) (int, error) {
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

	return n, nil
}

func (f *swiftFileCreationV3) Close() (err error) {
	defer func() {
		if err != nil {
			// Remove the temporary file from Swift if an error occurred
			_ = f.fs.c.ObjectDelete(f.fs.ctx, f.fs.container, f.name)
			// If an error has occurred when creating a new file, we should
			// also delete the file from the index.
			if f.olddoc == nil {
				_ = f.fs.Indexer.DeleteFileDoc(f.newdoc)
			}
		}
	}()

	if err = f.f.Close(); err != nil {
		if errors.Is(err, swift.ObjectCorrupted) {
			err = vfs.ErrInvalidHash
		}
		if f.meta != nil {
			(*f.meta).Abort(err)
			f.meta = nil
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

	// The actual check of the optionally given md5 hash is handled by the swift
	// library.
	if newdoc.MD5Sum == nil {
		var headers swift.Headers
		var md5sum []byte
		headers, err = f.f.Headers()
		if err != nil {
			return err
		}
		// Etags may be double-quoted
		etag := headers["Etag"]
		if l := len(etag); l >= 2 {
			if etag[0] == '"' {
				etag = etag[1:]
			}
			if etag[l-1] == '"' {
				etag = etag[:l-1]
			}
		}
		md5sum, err = hex.DecodeString(etag)
		if err != nil {
			return err
		}
		newdoc.MD5Sum = md5sum
	}

	if f.size < 0 {
		newdoc.ByteSize = written
	}

	if newdoc.ByteSize != written {
		return vfs.ErrContentLengthMismatch
	}

	lockerr := f.fs.mu.Lock()
	if lockerr != nil {
		return lockerr
	}
	defer f.fs.mu.Unlock()

	// Check again that a file with the same path does not exist. It can happen
	// when the same file is uploaded twice in parallel.
	if olddoc == nil {
		exists, err := f.fs.Indexer.DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return err
		}
		if exists {
			return os.ErrExist
		}
	}

	var newpath string
	newpath, err = f.fs.Indexer.FilePath(newdoc)
	if err != nil {
		return err
	}
	newdoc.Trashed = strings.HasPrefix(newpath, vfs.TrashDirName+"/")

	var v *vfs.Version
	if olddoc != nil {
		v = vfs.NewVersion(olddoc)
		err = f.fs.Indexer.UpdateFileDoc(olddoc, newdoc)
	} else if newdoc.ID() == "" {
		err = f.fs.Indexer.CreateFileDoc(newdoc)
	} else {
		err = f.fs.Indexer.CreateNamedFileDoc(newdoc)
	}
	if err != nil {
		return err
	}

	if v != nil {
		actionV, toClean, _ := vfs.FindVersionsToClean(f.fs, newdoc.DocID, v)
		if bytes.Equal(newdoc.MD5Sum, olddoc.MD5Sum) {
			actionV = vfs.CleanCandidateVersion
		}
		if actionV == vfs.KeepCandidateVersion {
			if errv := f.fs.Indexer.CreateVersion(v); errv != nil {
				actionV = vfs.CleanCandidateVersion
			}
		}
		if actionV == vfs.CleanCandidateVersion {
			internalID := v.DocID
			if parts := strings.SplitN(v.DocID, "/", 2); len(parts) > 1 {
				internalID = parts[1]
			}
			objName := MakeObjectNameV3(newdoc.DocID, internalID)
			if err := f.fs.c.ObjectDelete(f.fs.ctx, f.fs.container, objName); err != nil {
				f.fs.log.Warnf("Could not delete previous version %q: %s", objName, err.Error())
			}
		}
		for _, old := range toClean {
			if err := cleanOldVersion(f.fs, newdoc.DocID, old); err != nil {
				f.fs.log.Warnf("Could not delete old versions for %s: %s", newdoc.DocID, err.Error())
			}
		}
	}

	if f.capsize > 0 && f.size >= f.capsize {
		vfs.PushDiskQuotaAlert(f.fs, true)
	}

	return nil
}

func (sfs *swiftVFSV3) CleanOldVersion(fileID string, v *vfs.Version) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	return cleanOldVersion(sfs, fileID, v)
}

func cleanOldVersion(sfs *swiftVFSV3, fileID string, v *vfs.Version) error {
	if err := sfs.Indexer.DeleteVersion(v); err != nil {
		return err
	}
	internalID := v.DocID
	if parts := strings.SplitN(v.DocID, "/", 2); len(parts) > 1 {
		internalID = parts[1]
	}
	objName := MakeObjectNameV3(fileID, internalID)
	return sfs.c.ObjectDelete(sfs.ctx, sfs.container, objName)
}

func (sfs *swiftVFSV3) ClearOldVersions() error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	diskUsage, _ := sfs.Indexer.DiskUsage()
	versions, err := sfs.Indexer.AllVersions()
	if err != nil {
		return err
	}
	var objNames []string
	var destroyed int64
	for _, v := range versions {
		if parts := strings.SplitN(v.DocID, "/", 2); len(parts) > 1 {
			objNames = append(objNames, MakeObjectNameV3(parts[0], parts[1]))
		}
		destroyed += v.ByteSize
	}
	if err := sfs.Indexer.BatchDeleteVersions(versions); err != nil {
		return err
	}
	vfs.DiskQuotaAfterDestroy(sfs, diskUsage, destroyed)
	return deleteContainerFiles(sfs.ctx, sfs.c, sfs.container, objNames)
}

type swiftFileOpenV3 struct {
	f  *swift.ObjectOpenFile
	br *bytes.Reader
}

func (f *swiftFileOpenV3) Read(p []byte) (int, error) {
	return f.f.Read(p)
}

func (f *swiftFileOpenV3) ReadAt(p []byte, off int64) (int, error) {
	if f.br == nil {
		buf, err := io.ReadAll(f.f)
		if err != nil {
			return 0, err
		}
		f.br = bytes.NewReader(buf)
	}
	return f.br.ReadAt(p, off)
}

func (f *swiftFileOpenV3) Seek(offset int64, whence int) (int64, error) {
	n, err := f.f.Seek(context.Background(), offset, whence)
	if err != nil {
		logger.WithNamespace("vfsswift-v3").Warnf("Can't seek: %s", err)
	}
	return n, err
}

func (f *swiftFileOpenV3) Write(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f *swiftFileOpenV3) Close() error {
	return f.f.Close()
}

var (
	_ vfs.VFS  = &swiftVFSV3{}
	_ vfs.File = &swiftFileCreationV3{}
	_ vfs.File = &swiftFileOpenV3{}
)
