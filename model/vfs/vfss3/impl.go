// Package vfss3 is the implementation of the Virtual File System by using
// an S3-compatible object storage. The file contents are saved in S3 buckets,
// and the metadata are indexed in CouchDB.
package vfss3

import (
	"bytes"
	"context"
	"crypto/md5"

	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/s3util"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/gofrs/uuid/v5"
	"github.com/minio/minio-go/v7"
)

type s3VFS struct {
	vfs.Indexer
	vfs.DiskThresholder
	client      *minio.Client
	cluster     int
	domain      string
	prefix      string // DBPrefix — used as key prefix in the bucket
	contextName string
	ctx         context.Context
	bucket      string
	keyPrefix   string // prefix + "/"
	region      string
	mu          lock.ErrorRWLocker
	log         *logger.Entry
}

var bucketNameCleaner = regexp.MustCompile(`[^a-z0-9-]`)

// sanitizeBucketName produces a valid S3 bucket name component from an arbitrary string.
func sanitizeBucketName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, ".", "-")
	s = bucketNameCleaner.ReplaceAllString(s, "")
	// Collapse consecutive hyphens
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 37 {
		s = s[:37]
	}
	return s
}

// BucketName returns the S3 bucket name for a given orgID and bucket prefix.
func BucketName(orgID, bucketPrefix string) string {
	if orgID == "" {
		orgID = "default"
	}
	name := bucketPrefix + "-" + sanitizeBucketName(orgID)
	if len(name) > 63 {
		name = name[:63]
	}
	name = strings.TrimRight(name, "-")
	if len(name) < 3 {
		name += strings.Repeat("0", 3-len(name))
	}
	return name
}

// MakeObjectKey builds the S3 object key for a given file.
// It reuses the same virtual subfolder structure as Swift V3.
func MakeObjectKey(keyPrefix, docID, internalID string) string {
	return keyPrefix + makeObjectName(docID, internalID)
}

// makeObjectName builds the object name (without key prefix), identical to
// vfsswift.MakeObjectNameV3.
func makeObjectName(docID, internalID string) string {
	if len(docID) != 32 || len(internalID) != 16 {
		return docID + "/" + internalID
	}
	return docID[:22] + "/" + docID[22:27] + "/" + docID[27:] + "/" + internalID
}

func makeDocID(objName string) (string, string) {
	if len(objName) != 51 {
		parts := strings.SplitN(objName, "/", 2)
		if len(parts) < 2 {
			return objName, ""
		}
		return parts[0], parts[1]
	}
	return objName[:22] + objName[23:28] + objName[29:34], objName[35:]
}

// NewInternalID returns a random string that can be used as an internal_vfs_id.
func NewInternalID() string {
	return utils.RandomString(16)
}

// New returns a vfs.VFS instance backed by an S3-compatible object store.
func New(db vfs.Prefixer, index vfs.Indexer, disk vfs.DiskThresholder, mu lock.ErrorRWLocker) (vfs.VFS, error) {
	client := config.GetS3Client()
	bucketPrefix := config.GetS3BucketPrefix()

	orgID := ""
	if inst, ok := db.(interface{ GetOrgID() string }); ok {
		orgID = inst.GetOrgID()
	}
	bucket := BucketName(orgID, bucketPrefix)
	dbPrefix := db.DBPrefix()
	if dbPrefix == "" {
		return nil, fmt.Errorf("vfss3: empty DBPrefix")
	}

	return &s3VFS{
		Indexer:         index,
		DiskThresholder: disk,
		client:          client,
		cluster:         db.DBCluster(),
		domain:          db.DomainName(),
		prefix:          dbPrefix,
		contextName:     db.GetContextName(),
		ctx:             context.Background(),
		bucket:          bucket,
		keyPrefix:       dbPrefix + "/",
		region:          config.GetS3Region(),
		mu:              mu,
		log:             logger.WithDomain(db.DomainName()).WithNamespace("vfss3"),
	}, nil
}

func (sfs *s3VFS) MaxFileSize() int64 {
	return -1 // no per-file limit — S3 multipart handles large files transparently
}

func (sfs *s3VFS) DBCluster() int {
	return sfs.cluster
}

func (sfs *s3VFS) DBPrefix() string {
	return sfs.prefix
}

func (sfs *s3VFS) DomainName() string {
	return sfs.domain
}

func (sfs *s3VFS) GetContextName() string {
	return sfs.contextName
}

func (sfs *s3VFS) GetIndexer() vfs.Indexer {
	return sfs.Indexer
}

func (sfs *s3VFS) UseSharingIndexer(index vfs.Indexer) vfs.VFS {
	return &s3VFS{
		Indexer:         index,
		DiskThresholder: sfs.DiskThresholder,
		client:          sfs.client,
		cluster:         sfs.cluster,
		domain:          sfs.domain,
		prefix:          sfs.prefix,
		contextName:     sfs.contextName,
		ctx:             context.Background(),
		bucket:          sfs.bucket,
		keyPrefix:       sfs.keyPrefix,
		region:          sfs.region,
		mu:              sfs.mu,
		log:             sfs.log,
	}
}

func (sfs *s3VFS) InitFs() error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	if err := sfs.Indexer.InitIndex(); err != nil {
		return err
	}
	err := sfs.client.MakeBucket(sfs.ctx, sfs.bucket, minio.MakeBucketOptions{
		Region: sfs.region,
	})
	if err != nil {
		code := minio.ToErrorResponse(err).Code
		if code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists" {
			return nil
		}
		sfs.log.Errorf("Could not create bucket %q: %s", sfs.bucket, err.Error())
		return err
	}
	sfs.log.Infof("Created bucket %q", sfs.bucket)
	return nil
}

func (sfs *s3VFS) Delete() error {
	sfs.log.Infof("Deleting all objects with prefix %q in bucket %q", sfs.keyPrefix, sfs.bucket)
	return s3util.DeletePrefixObjects(sfs.ctx, sfs.client, sfs.bucket, sfs.keyPrefix)
}

func (sfs *s3VFS) CreateDir(doc *vfs.DirDoc) error {
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

// putResult is the result sent back from the background PutObject goroutine.
type putResult struct {
	info minio.UploadInfo
	err  error
}

func (sfs *s3VFS) CreateFile(newdoc, olddoc *vfs.FileDoc, opts ...vfs.CreateOptions) (vfs.File, error) {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.Unlock()

	newsize, maxsize, capsize, err := vfs.CheckAvailableDiskSpace(sfs, newdoc)
	if err != nil {
		return nil, err
	}
	// CheckAvailableDiskSpace already returns ErrFileTooBig / ErrMaxFileSize
	// when the upload would exceed the per-file or quota budget. The
	// streaming-time check below (s3FileCreation.Write) re-checks against
	// maxsize once the actual byte count is known.

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
	objKey := MakeObjectKey(sfs.keyPrefix, newdoc.DocID, newdoc.InternalID)

	// Use a pipe: writes go into pw, the PutObject goroutine reads from pr.
	pr, pw := io.Pipe()

	uploadSize := newdoc.ByteSize
	if uploadSize < 0 {
		uploadSize = -1
	}

	resultCh := make(chan putResult, 1)
	go func() {
		info, err := sfs.client.PutObject(sfs.ctx, sfs.bucket, objKey, pr, uploadSize, minio.PutObjectOptions{
			ContentType: newdoc.Mime,
			PartSize:    5 * 1024 * 1024, // 5 MiB
			NumThreads:  1,
		})
		// Propagate the outcome to the writer side: if PutObject errored before
		// draining the pipe, an in-flight Write would otherwise block forever.
		_ = pr.CloseWithError(err)
		resultCh <- putResult{info: info, err: err}
	}()

	extractor := vfs.NewMetaExtractor(newdoc)

	return &s3FileCreation{
		fs:       sfs,
		pw:       pw,
		resultCh: resultCh,
		newdoc:   newdoc,
		olddoc:   olddoc,
		objKey:   objKey,
		w:        0,
		size:     newsize,
		maxsize:  maxsize,
		capsize:  capsize,
		meta:     extractor,
		md5H:     md5.New(),
	}, nil
}

func (sfs *s3VFS) CopyFile(olddoc, newdoc *vfs.FileDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()

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

	srcKey := MakeObjectKey(sfs.keyPrefix, olddoc.DocID, olddoc.InternalID)
	dstKey := MakeObjectKey(sfs.keyPrefix, newdoc.DocID, newdoc.InternalID)

	if _, err := sfs.client.CopyObject(sfs.ctx,
		minio.CopyDestOptions{Bucket: sfs.bucket, Object: dstKey},
		minio.CopySrcOptions{Bucket: sfs.bucket, Object: srcKey},
	); err != nil {
		return err
	}
	if err := sfs.Indexer.CreateNamedFileDoc(newdoc); err != nil {
		_ = sfs.client.RemoveObject(sfs.ctx, sfs.bucket, dstKey, minio.RemoveObjectOptions{})
		return err
	}

	if capsize > 0 && newsize >= capsize {
		vfs.PushDiskQuotaAlert(sfs, true)
	}

	return nil
}

func (sfs *s3VFS) DissociateFile(src, dst *vfs.FileDoc) error {
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

	srcKey := MakeObjectKey(sfs.keyPrefix, src.DocID, src.InternalID)
	dstKey := MakeObjectKey(sfs.keyPrefix, dst.DocID, dst.InternalID)

	if _, err := sfs.client.CopyObject(sfs.ctx,
		minio.CopyDestOptions{Bucket: sfs.bucket, Object: dstKey},
		minio.CopySrcOptions{Bucket: sfs.bucket, Object: srcKey},
	); err != nil {
		return err
	}
	if err := sfs.Indexer.CreateNamedFileDoc(dst); err != nil {
		_ = sfs.client.RemoveObject(sfs.ctx, sfs.bucket, dstKey, minio.RemoveObjectOptions{})
		return err
	}

	return sfs.destroyFileLocked(src)
}

func (sfs *s3VFS) DissociateDir(src, dst *vfs.DirDoc) error {
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

func (sfs *s3VFS) destroyDir(doc *vfs.DirDoc, push func(vfs.TrashJournal) error, onlyContent bool) error {
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
		objNames[i] = MakeObjectKey(sfs.keyPrefix, file.DocID, file.InternalID)
	}
	return push(vfs.TrashJournal{
		FileIDs:     ids,
		ObjectNames: objNames,
	})
}

func (sfs *s3VFS) DestroyDirContent(doc *vfs.DirDoc, push func(vfs.TrashJournal) error) error {
	return sfs.destroyDir(doc, push, true)
}

func (sfs *s3VFS) DestroyDirAndContent(doc *vfs.DirDoc, push func(vfs.TrashJournal) error) error {
	return sfs.destroyDir(doc, push, false)
}

func (sfs *s3VFS) DestroyFile(doc *vfs.FileDoc) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	return sfs.destroyFileLocked(doc)
}

func (sfs *s3VFS) destroyFileLocked(doc *vfs.FileDoc) error {
	diskUsage, _ := sfs.Indexer.DiskUsage()
	objNames := []string{
		MakeObjectKey(sfs.keyPrefix, doc.DocID, doc.InternalID),
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
			objNames = append(objNames, MakeObjectKey(sfs.keyPrefix, doc.DocID, internalID))
			destroyed += v.ByteSize
		}
		if err := sfs.Indexer.BatchDeleteVersions(versions); err != nil {
			sfs.log.Warnf("DestroyFile failed on BatchDeleteVersions: %s", err)
		}
	}
	if err := s3util.DeleteObjects(sfs.ctx, sfs.client, sfs.bucket, objNames); err != nil {
		sfs.log.Warnf("DestroyFile failed on deleteObjects: %s", err)
	}
	vfs.DiskQuotaAfterDestroy(sfs, diskUsage, destroyed)
	return nil
}

func (sfs *s3VFS) EnsureErased(journal vfs.TrashJournal) error {
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
				errm = errors.Join(errm, err)
			}
			continue
		}
		for _, v := range versions {
			internalID := v.DocID
			if parts := strings.SplitN(v.DocID, "/", 2); len(parts) > 1 {
				internalID = parts[1]
			}
			objNames = append(objNames, MakeObjectKey(sfs.keyPrefix, fileID, internalID))
			destroyed += v.ByteSize
		}
		allVersions = append(allVersions, versions...)
	}
	if err := sfs.Indexer.BatchDeleteVersions(allVersions); err != nil {
		sfs.log.Warnf("EnsureErased failed on BatchDeleteVersions: %s", err)
		errm = errors.Join(errm, err)
	}
	if err := s3util.DeleteObjects(sfs.ctx, sfs.client, sfs.bucket, objNames); err != nil {
		sfs.log.Warnf("EnsureErased failed on deleteObjects: %s", err)
		errm = errors.Join(errm, err)
	}
	vfs.DiskQuotaAfterDestroy(sfs, diskUsage, destroyed)
	return errm
}

func (sfs *s3VFS) OpenFile(doc *vfs.FileDoc) (vfs.File, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	objKey := MakeObjectKey(sfs.keyPrefix, doc.DocID, doc.InternalID)
	obj, err := sfs.client.GetObject(sfs.ctx, sfs.bucket, objKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// Stat the object to detect if it exists.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return &s3FileOpen{obj}, nil
}

func (sfs *s3VFS) OpenFileVersion(doc *vfs.FileDoc, version *vfs.Version) (vfs.File, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	internalID := version.DocID
	if parts := strings.SplitN(version.DocID, "/", 2); len(parts) > 1 {
		internalID = parts[1]
	}
	objKey := MakeObjectKey(sfs.keyPrefix, doc.DocID, internalID)
	obj, err := sfs.client.GetObject(sfs.ctx, sfs.bucket, objKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return &s3FileOpen{obj}, nil
}

func (sfs *s3VFS) ImportFileVersion(version *vfs.Version, content io.ReadCloser) error {
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
	objKey := MakeObjectKey(sfs.keyPrefix, parts[0], parts[1])

	_, err := sfs.client.PutObject(sfs.ctx, sfs.bucket, objKey, content, version.ByteSize, minio.PutObjectOptions{
		ContentType:    "application/octet-stream",
		SendContentMd5: true,
	})
	if errc := content.Close(); err == nil {
		err = errc
	}
	if err != nil {
		return err
	}

	return sfs.Indexer.CreateVersion(version)
}

// WriteContentAt streams content into the object backing the (docID, internalID)
// key, creating NO CouchDB document. It is used by storage migration, which
// preserves the shared index and only moves object bytes. size may be -1 when
// unknown (falls back to multipart).
func (sfs *s3VFS) WriteContentAt(docID, internalID string, content io.Reader, size int64) error {
	objKey := MakeObjectKey(sfs.keyPrefix, docID, internalID)
	_, err := sfs.client.PutObject(sfs.ctx, sfs.bucket, objKey, content, size, minio.PutObjectOptions{
		ContentType:    "application/octet-stream",
		SendContentMd5: true,
	})
	return err
}

// StatContentAt returns the byte size of the object backing the (docID,
// internalID) key, without touching CouchDB. It returns os.ErrNotExist when
// the object is absent. Used by storage migration to verify a copy landed on
// the target before flipping the instance's backend flag.
func (sfs *s3VFS) StatContentAt(docID, internalID string) (int64, error) {
	objKey := MakeObjectKey(sfs.keyPrefix, docID, internalID)
	info, err := sfs.client.StatObject(sfs.ctx, sfs.bucket, objKey, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return 0, os.ErrNotExist
		}
		return 0, err
	}
	return info.Size, nil
}

func (sfs *s3VFS) RevertFileVersion(doc *vfs.FileDoc, version *vfs.Version) error {
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

func (sfs *s3VFS) CleanOldVersion(fileID string, v *vfs.Version) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()
	return sfs.cleanOldVersion(fileID, v)
}

func (sfs *s3VFS) cleanOldVersion(fileID string, v *vfs.Version) error {
	if err := sfs.Indexer.DeleteVersion(v); err != nil {
		return err
	}
	internalID := v.DocID
	if parts := strings.SplitN(v.DocID, "/", 2); len(parts) > 1 {
		internalID = parts[1]
	}
	objKey := MakeObjectKey(sfs.keyPrefix, fileID, internalID)
	return sfs.client.RemoveObject(sfs.ctx, sfs.bucket, objKey, minio.RemoveObjectOptions{})
}

func (sfs *s3VFS) ClearOldVersions() error {
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
			objNames = append(objNames, MakeObjectKey(sfs.keyPrefix, parts[0], parts[1]))
		}
		destroyed += v.ByteSize
	}
	if err := sfs.Indexer.BatchDeleteVersions(versions); err != nil {
		return err
	}
	vfs.DiskQuotaAfterDestroy(sfs, diskUsage, destroyed)
	return s3util.DeleteObjects(sfs.ctx, sfs.client, sfs.bucket, objNames)
}

func (sfs *s3VFS) CopyFileFromOtherFS(
	newdoc, olddoc *vfs.FileDoc,
	srcFS vfs.Fs,
	srcDoc *vfs.FileDoc,
) error {
	if lockerr := sfs.mu.Lock(); lockerr != nil {
		return lockerr
	}
	defer sfs.mu.Unlock()

	newsize, _, capsize, err := vfs.CheckAvailableDiskSpace(sfs, newdoc)
	if err != nil {
		return err
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

	dstKey := MakeObjectKey(sfs.keyPrefix, newdoc.DocID, newdoc.InternalID)

	// Try server-side copy if the source is also an s3VFS on the same client.
	if srcS3, ok := srcFS.(*s3VFS); ok {
		srcKey := MakeObjectKey(srcS3.keyPrefix, srcDoc.DocID, srcDoc.InternalID)
		if _, err := sfs.client.CopyObject(sfs.ctx,
			minio.CopyDestOptions{Bucket: sfs.bucket, Object: dstKey},
			minio.CopySrcOptions{Bucket: srcS3.bucket, Object: srcKey},
		); err != nil {
			return err
		}
	} else {
		// Stream from the source FS.
		srcFile, err := srcFS.OpenFile(srcDoc)
		if err != nil {
			return err
		}
		_, err = sfs.client.PutObject(sfs.ctx, sfs.bucket, dstKey, srcFile, srcDoc.ByteSize, minio.PutObjectOptions{
			ContentType: srcDoc.Mime,
		})
		if errc := srcFile.Close(); err == nil {
			err = errc
		}
		if err != nil {
			return err
		}
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
			objKey := MakeObjectKey(sfs.keyPrefix, newdoc.DocID, internalID)
			_ = sfs.client.RemoveObject(sfs.ctx, sfs.bucket, objKey, minio.RemoveObjectOptions{})
		}
		for _, old := range toClean {
			_ = sfs.cleanOldVersion(newdoc.DocID, old)
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
func (sfs *s3VFS) UpdateFileDoc(olddoc, newdoc *vfs.FileDoc) error {
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

// UpdateDirDoc calls the indexer UpdateDirDoc function and adds a few checks
// before actually calling this method:
//   - locks the filesystem for writing
//   - checks that we don't move a directory to one of its descendant
//   - checks in case we have a move operation that the new path is available
//
// @override Indexer.UpdateDirDoc
func (sfs *s3VFS) UpdateDirDoc(olddoc, newdoc *vfs.DirDoc) error {
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

func (sfs *s3VFS) DirByID(fileID string) (*vfs.DirDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.DirByID(fileID)
}

func (sfs *s3VFS) DirByPath(name string) (*vfs.DirDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.DirByPath(name)
}

func (sfs *s3VFS) FileByID(fileID string) (*vfs.FileDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.FileByID(fileID)
}

func (sfs *s3VFS) FileByPath(name string) (*vfs.FileDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.FileByPath(name)
}

func (sfs *s3VFS) FilePath(doc *vfs.FileDoc) (string, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return "", lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.FilePath(doc)
}

func (sfs *s3VFS) DirOrFileByID(fileID string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.DirOrFileByID(fileID)
}

func (sfs *s3VFS) DirOrFileByPath(name string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	if lockerr := sfs.mu.RLock(); lockerr != nil {
		return nil, nil, lockerr
	}
	defer sfs.mu.RUnlock()
	return sfs.Indexer.DirOrFileByPath(name)
}

// s3FileCreation represents a file open for writing. It is used to create
// a file or to modify the content of a file.
type s3FileCreation struct {
	fs       *s3VFS
	pw       *io.PipeWriter
	resultCh chan putResult
	newdoc   *vfs.FileDoc
	olddoc   *vfs.FileDoc
	objKey   string
	w        int64
	size     int64
	maxsize  int64
	capsize  int64
	meta     *vfs.MetaExtractor
	md5H     hash.Hash
	err      error
}

func (f *s3FileCreation) Read(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f *s3FileCreation) ReadAt(p []byte, off int64) (int, error) {
	return 0, os.ErrInvalid
}

func (f *s3FileCreation) Seek(offset int64, whence int) (int64, error) {
	return 0, os.ErrInvalid
}

func (f *s3FileCreation) Write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}

	if f.meta != nil {
		if _, err := (*f.meta).Write(p); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			(*f.meta).Abort(err)
			f.meta = nil
		}
	}

	// Write to local MD5 hash
	_, _ = f.md5H.Write(p)

	n, err := f.pw.Write(p)
	if err != nil {
		f.err = err
		return n, err
	}

	f.w += int64(n)
	if f.maxsize >= 0 && f.w > f.maxsize {
		f.err = vfs.ErrFileTooBig
		_ = f.pw.CloseWithError(f.err)
		return n, f.err
	}

	if f.size >= 0 && f.w > f.size {
		f.err = vfs.ErrContentLengthMismatch
		_ = f.pw.CloseWithError(f.err)
		return n, f.err
	}

	return n, nil
}

func (f *s3FileCreation) Close() (err error) {
	defer func() {
		if err != nil {
			// Remove the object from S3 if an error occurred
			_ = f.fs.client.RemoveObject(f.fs.ctx, f.fs.bucket, f.objKey, minio.RemoveObjectOptions{})
			// If an error has occurred when creating a new file, we should
			// also delete the file from the index.
			if f.olddoc == nil {
				_ = f.fs.Indexer.DeleteFileDoc(f.newdoc)
			}
		}
	}()

	// Close the pipe writer to signal EOF to PutObject
	if err = f.pw.Close(); err != nil {
		if f.meta != nil {
			(*f.meta).Abort(err)
			f.meta = nil
		}
		if f.err == nil {
			f.err = err
		}
	}

	// Wait for the PutObject goroutine to finish
	result := <-f.resultCh

	if result.err != nil {
		if f.meta != nil {
			(*f.meta).Abort(result.err)
			f.meta = nil
		}
		if f.err == nil {
			f.err = result.err
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

	// Verify or compute MD5 checksum.
	// The local md5H hash is always computed from the same data stream that
	// goes to S3 (via the Write method), so it is authoritative.
	localMD5 := f.md5H.Sum(nil)
	if newdoc.MD5Sum != nil {
		// The caller provided an expected hash — verify it matches what was written.
		if !bytes.Equal(newdoc.MD5Sum, localMD5) {
			return vfs.ErrInvalidHash
		}
	} else {
		newdoc.MD5Sum = localMD5
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
			objKey := MakeObjectKey(f.fs.keyPrefix, newdoc.DocID, internalID)
			if err := f.fs.client.RemoveObject(f.fs.ctx, f.fs.bucket, objKey, minio.RemoveObjectOptions{}); err != nil {
				f.fs.log.Warnf("Could not delete previous version %q: %s", objKey, err.Error())
			}
		}
		for _, old := range toClean {
			if err := f.fs.cleanOldVersion(newdoc.DocID, old); err != nil {
				f.fs.log.Warnf("Could not delete old versions for %s: %s", newdoc.DocID, err.Error())
			}
		}
	}

	if f.capsize > 0 && f.size >= f.capsize {
		vfs.PushDiskQuotaAlert(f.fs, true)
	}

	return nil
}

// s3FileOpen represents a file open for reading.
type s3FileOpen struct {
	obj *minio.Object
}

func (f *s3FileOpen) Read(p []byte) (int, error) {
	return f.obj.Read(p)
}

func (f *s3FileOpen) ReadAt(p []byte, off int64) (int, error) {
	return f.obj.ReadAt(p, off)
}

func (f *s3FileOpen) Seek(offset int64, whence int) (int64, error) {
	return f.obj.Seek(offset, whence)
}

func (f *s3FileOpen) Write(p []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (f *s3FileOpen) Close() error {
	return f.obj.Close()
}

var (
	_ vfs.VFS  = &s3VFS{}
	_ vfs.File = &s3FileCreation{}
	_ vfs.File = &s3FileOpen{}
)
