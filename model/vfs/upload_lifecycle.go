package vfs

import (
	"bytes"
	"crypto/md5"
	crand "crypto/rand"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/gofrs/uuid/v5"
)

// NewUpload creates a resumable upload for the given file. olddoc is the
// current version of the file when its content is replaced, and oldRev the
// revision expected by the client, if any. length is -1 when unknown.
func NewUpload(fs VFS, newdoc, olddoc *FileDoc, oldRev string, length int64) (*Upload, error) {
	store, err := uploadStorage(fs)
	if err != nil {
		return nil, err
	}
	cfg := uploadConfig()

	uploads, err := activeUploads(fs)
	if err != nil {
		return nil, err
	}
	active := 0
	for _, u := range uploads {
		if !u.Completed && !u.expired() {
			active++
		}
	}
	if active >= cfg.MaxPerInstance {
		return nil, ErrTooManyUploads
	}

	limit, err := uploadSpaceLimit(fs, uploads, "")
	if err != nil {
		return nil, err
	}
	if length > limit {
		return nil, ErrFileTooBig
	}

	if olddoc != nil {
		newdoc.SetID(olddoc.ID())
		newdoc.CreatedAt = olddoc.CreatedAt
	}
	newpath, err := fs.FilePath(newdoc)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(newpath, TrashDirName+"/") {
		return nil, ErrParentInTrash
	}
	if olddoc == nil {
		exists, err := fs.DirChildExists(newdoc.DirID, newdoc.DocName)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, os.ErrExist
		}
		uid, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		newdoc.DocID = uid.String()
	}

	state, err := md5.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	u := &Upload{
		UploadID:  strings.ToLower(crand.Text()),
		File:      newdoc,
		OldRev:    oldRev,
		Length:    length,
		MD5State:  state,
		Owner:     crand.Text(),
		CreatedAt: now,
		ExpiresAt: now.Add(cfg.MaxAge),
	}
	if olddoc != nil {
		u.OldFileID = olddoc.ID()
	}
	if err := store.StartUpload(u); err != nil {
		return nil, err
	}
	if err := couchdb.CreateNamedDocWithDB(fs, u); err != nil {
		_ = store.AbortUpload(u)
		return nil, err
	}
	return u, nil
}

// GetUpload returns the upload with the given ID, or ErrUploadNotFound if it
// does not exist or has expired.
func GetUpload(fs VFS, id string) (*Upload, error) {
	u := &Upload{}
	if err := couchdb.GetDoc(fs, consts.FilesUploads, id, u); err != nil {
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			return nil, ErrUploadNotFound
		}
		return nil, err
	}
	if u.expired() {
		return nil, ErrUploadNotFound
	}
	return u, nil
}

// ClaimUpload loads an upload and makes the calling request its owner. An
// older request still running for this upload will stop before writing again
// (draft-ietf-httpbis-resumable-upload §10).
func ClaimUpload(fs VFS, id string) (*Upload, error) {
	var claimed *Upload
	err := withUploadLock(fs, id, func() error {
		u, err := GetUpload(fs, id)
		if err != nil {
			return err
		}
		if !u.Completed {
			u.Owner = crand.Text()
			if err := couchdb.UpdateDoc(fs, u); err != nil {
				return err
			}
		}
		claimed = u
		return nil
	})
	return claimed, err
}

// SetUploadLength records the length of the content, once it is known.
func SetUploadLength(fs VFS, u *Upload, length int64) error {
	return u.locked(fs, func(fresh *Upload) error {
		if fresh.Length >= 0 && fresh.Length != length {
			return ErrUploadLength
		}
		if fresh.Offset > length {
			return deactivateLocked(fs, fresh, ErrUploadLength)
		}
		fresh.Length = length
		return couchdb.UpdateDoc(fs, fresh)
	})
}

// CancelUpload removes an upload and its content. A completed upload only
// loses its record: the file stays.
func CancelUpload(fs VFS, id string) error {
	return withUploadLock(fs, id, func() error {
		u, err := GetUpload(fs, id)
		if err != nil {
			return err
		}
		return removeUpload(fs, u)
	})
}

// CleanUploads removes the uploads that have expired. It returns the number of
// removed uploads.
func CleanUploads(fs VFS) (int, error) {
	return removeUploads(fs, (*Upload).expired)
}

// RemoveAllUploads removes all the uploads, with the content of those in
// progress. The storage migration calls it before switching the storage of
// an instance: the state of an upload points to the parts of the source.
func RemoveAllUploads(fs VFS) (int, error) {
	return removeUploads(fs, func(*Upload) bool { return true })
}

func removeUploads(fs VFS, selected func(u *Upload) bool) (int, error) {
	uploads, err := listUploads(fs)
	if err != nil {
		return 0, err
	}
	var errm error
	removed := 0
	for _, u := range uploads {
		if !selected(u) {
			continue
		}
		err := withUploadLock(fs, u.UploadID, func() error {
			fresh := &Upload{}
			if err := couchdb.GetDoc(fs, consts.FilesUploads, u.UploadID, fresh); err != nil {
				return err
			}
			if !selected(fresh) {
				return nil
			}
			removed++
			return removeUpload(fs, fresh)
		})
		if err != nil && !couchdb.IsNotFoundError(err) {
			errm = errors.Join(errm, err)
		}
	}
	return removed, errm
}

// UploadSpaceLimit returns the maximal size for the content of the given
// upload, from the maximal file size and the quota (the other uploads in
// progress count in the quota).
func UploadSpaceLimit(fs VFS, u *Upload) (int64, error) {
	uploads, err := activeUploads(fs)
	if err != nil {
		return 0, err
	}
	return uploadSpaceLimit(fs, uploads, u.UploadID)
}

func uploadSpaceLimit(fs VFS, uploads []*Upload, exclude string) (int64, error) {
	limit := int64(math.MaxInt64)
	if maxsize := fs.MaxFileSize(); maxsize > 0 {
		limit = maxsize
	}
	quota := fs.DiskQuota()
	if quota <= 0 {
		return limit, nil
	}
	usage, err := fs.DiskUsage()
	if err != nil {
		return 0, err
	}
	available := quota - usage
	for _, u := range uploads {
		if u.Completed || u.expired() || u.UploadID == exclude {
			continue
		}
		available -= max(u.Length, u.Offset)
	}
	return max(min(limit, available), 0), nil
}

// locked runs fn with the upload lock held, on a fresh copy of the upload,
// only if the request is still the owner of the upload. u is updated with the
// state saved by fn.
func (u *Upload) locked(fs VFS, fn func(fresh *Upload) error) error {
	return withUploadLock(fs, u.UploadID, func() error {
		fresh, err := GetUpload(fs, u.UploadID)
		if err != nil {
			return err
		}
		if fresh.Completed || fresh.Owner != u.Owner {
			return ErrUploadTakenOver
		}
		if err := fn(fresh); err != nil {
			return err
		}
		*u = *fresh
		return nil
	})
}

func withUploadLock(fs VFS, id string, fn func() error) error {
	// A fixed number of lock names per instance: the lock getters keep every
	// name they have seen.
	name := fmt.Sprintf("vfs/uploads/%d", crc32.ChecksumIEEE([]byte(id))%8)
	mu := config.Lock().LongOperation(fs, name)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()
	return fn()
}

// deactivateUpload removes an upload after an error that prevents resuming it.
func deactivateUpload(fs VFS, u *Upload, cause error) error {
	err := u.locked(fs, func(fresh *Upload) error {
		return deactivateLocked(fs, fresh, cause)
	})
	if errors.Is(err, cause) {
		return cause
	}
	return errors.Join(cause, err)
}

func deactivateLocked(fs VFS, u *Upload, cause error) error {
	if err := removeUpload(fs, u); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// committedFile returns the file when the assembled content of the upload is
// already its content: the commit has succeeded, but the upload has not been
// marked as completed. Its content must not be aborted.
func committedFile(fs VFS, u *Upload) *FileDoc {
	if !u.Assembled || u.File == nil {
		return nil
	}
	doc, err := fs.FileByID(u.File.ID())
	if err != nil {
		return nil
	}
	if u.File.InternalID != "" {
		if doc.InternalID == u.File.InternalID {
			return doc
		}
		// The file may have a newer content since: the one of the upload is
		// then a version.
		versions, err := VersionsFor(fs, doc.ID())
		if err != nil {
			return nil
		}
		for _, v := range versions {
			if v.DocID == doc.ID()+"/"+u.File.InternalID {
				return doc
			}
		}
		return nil
	}
	// The local file system has no internal ID: the content is known by its
	// size and md5.
	if doc.ByteSize != u.File.ByteSize || !bytes.Equal(doc.MD5Sum, u.File.MD5Sum) {
		return nil
	}
	return doc
}

func removeUpload(fs VFS, u *Upload) error {
	if !u.Completed && committedFile(fs, u) == nil {
		store, err := uploadStorage(fs)
		if err != nil {
			return err
		}
		if err := store.AbortUpload(u); err != nil {
			return err
		}
	}
	return couchdb.DeleteDoc(fs, u)
}

// activeUploads returns the uploads that are not completed. It does not read
// the records of the completed uploads, kept until they expire.
func activeUploads(fs VFS) ([]*Upload, error) {
	var uploads []*Upload
	req := &couchdb.FindRequest{
		UseIndex: "by-completed",
		Selector: mango.Equal("completed", false),
		Limit:    1000,
	}
	err := couchdb.FindDocs(fs, consts.FilesUploads, req, &uploads)
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return nil, err
	}
	return uploads, nil
}

func listUploads(fs VFS) ([]*Upload, error) {
	var uploads []*Upload
	err := couchdb.ForeachDocs(fs, consts.FilesUploads, func(_ string, data json.RawMessage) error {
		u := &Upload{}
		if err := json.Unmarshal(data, u); err != nil {
			return err
		}
		uploads = append(uploads, u)
		return nil
	})
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return nil, err
	}
	return uploads, nil
}
