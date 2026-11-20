package vfs

import (
	"bytes"
	"crypto/md5"
	crand "crypto/rand"
	"encoding"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/gofrs/uuid/v5"
	"golang.org/x/sync/singleflight"
)

// The state of an upload is guarded by the _rev of its document: each change
// reads a fresh copy, checks that it still applies, and saves it. On a
// conflict, the change is evaluated again on the new state. No lock is held
// while the content is written to the storage.

// errNotSelected tells that a document has changed and no longer needs the
// change.
var errNotSelected = errors.New("upload not selected")

// maxUpdateTries bounds the retries on conflicts. Each conflict means that
// another request has made progress.
const maxUpdateTries = 20

// NewUpload creates a resumable upload for the given file. olddoc is the
// current version of the file when its content is replaced, and oldRev the
// revision expected by the client, if any. length is -1 when unknown.
func NewUpload(fs VFS, newdoc, olddoc *FileDoc, oldRev string, length int64) (*Upload, error) {
	return newUpload(fs, newdoc, olddoc, oldRev, length, false)
}

// NewUploadByLink creates a resumable upload for a visitor of a sharing by
// link. Its announced length does not reserve space: only its written bytes
// count in the space left for the uploads of the owner.
func NewUploadByLink(fs VFS, newdoc, olddoc *FileDoc, oldRev string, length int64) (*Upload, error) {
	return newUpload(fs, newdoc, olddoc, oldRev, length, true)
}

func newUpload(fs VFS, newdoc, olddoc *FileDoc, oldRev string, length int64, byLink bool) (*Upload, error) {
	store, err := uploadStorage(fs)
	if err != nil {
		return nil, err
	}
	cfg := uploadConfig()

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
		ByLink:    byLink,
		CreatedAt: now,
		ExpiresAt: now.Add(cfg.MaxAge),
	}
	if olddoc != nil {
		u.OldFileID = olddoc.ID()
	}
	if err := store.StartUpload(u); err != nil {
		return nil, err
	}
	if err := createWithinLimits(fs, u, cfg); err != nil {
		_ = store.AbortUpload(u)
		return nil, err
	}
	return u, nil
}

// createWithinLimits counts the uploads in progress and saves the new one
// under a lock of the instance, held only for these reads and this write:
// uploads created at the same time cannot all pass the limits.
func createWithinLimits(fs VFS, u *Upload, cfg config.FsResumableUploads) error {
	mu := config.Lock().ReadWrite(fs, "vfs/uploads")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	uploads, err := activeUploads(fs)
	if err != nil {
		return err
	}
	active := 0
	for _, other := range uploads {
		if other.active() {
			active++
		}
	}
	// All the uploads are made by the account of the owner of the instance.
	if active >= cfg.MaxPerAccount || active >= cfg.MaxPerInstance {
		return ErrTooManyUploads
	}
	limit, err := uploadSpaceLimit(fs, uploads, "")
	if err != nil {
		return err
	}
	if u.Length > limit {
		return ErrFileTooBig
	}
	return createUploadDoc(fs, u)
}

// uploadsIndex finds the uploads in progress.
var uploadsIndex = mango.MakeIndex(consts.FilesUploads, "by-completed", mango.IndexDef{Fields: []string{"completed"}})

// createUploadDoc saves a new upload. The database and its index are created
// with the first upload of the instance, and only then: the other requests
// consider a missing database as having no upload.
func createUploadDoc(fs VFS, u *Upload) error {
	err := couchdb.CreateNamedDoc(fs, u)
	if !couchdb.IsNoDatabaseError(err) {
		return err
	}
	// Two first uploads can create the database at the same time.
	if err := couchdb.CreateDB(fs, consts.FilesUploads); err != nil && !couchdb.IsFileExists(err) {
		return err
	}
	if err := couchdb.DefineIndex(fs, uploadsIndex); err != nil {
		return err
	}
	return couchdb.CreateNamedDoc(fs, u)
}

func (u *Upload) active() bool {
	return !u.Completed && !u.Removed && !u.expired()
}

func getUploadDoc(fs VFS, id string) (*Upload, error) {
	u := &Upload{}
	if err := couchdb.GetDoc(fs, consts.FilesUploads, id, u); err != nil {
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			return nil, ErrUploadNotFound
		}
		return nil, err
	}
	return u, nil
}

// GetUpload returns the upload with the given ID, or ErrUploadNotFound if it
// does not exist, has expired or is being removed.
func GetUpload(fs VFS, id string) (*Upload, error) {
	u, err := getUploadDoc(fs, id)
	if err != nil {
		return nil, err
	}
	if u.Removed || u.expired() {
		return nil, ErrUploadNotFound
	}
	return u, nil
}

// updateUpload applies change to a fresh copy of the upload and saves it,
// again on a conflict. It returns the saved state.
func updateUpload(fs VFS, id string, change func(fresh *Upload) error) (*Upload, error) {
	var err error
	for i := 0; i < maxUpdateTries; i++ {
		var fresh *Upload
		fresh, err = GetUpload(fs, id)
		if err != nil {
			return nil, err
		}
		if err = change(fresh); err != nil {
			return nil, err
		}
		err = couchdb.UpdateDoc(fs, fresh)
		if couchdb.IsConflictError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return fresh, nil
	}
	return nil, err
}

// update applies change to the upload, only if the request is still its
// owner. u is updated with the saved state.
func (u *Upload) update(fs VFS, change func(fresh *Upload) error) error {
	saved, err := updateUpload(fs, u.UploadID, func(fresh *Upload) error {
		if fresh.Completed || fresh.Owner != u.Owner {
			return ErrUploadTakenOver
		}
		return change(fresh)
	})
	if err != nil {
		return err
	}
	*u = *saved
	return nil
}

// claimWait bounds the time a new request waits for an older one to save the
// bytes it has received.
var claimWait = 5 * time.Second

// pollUpload reads the upload while a new request waits for an older one.
var pollUpload = GetUpload

// ClaimUpload loads an upload and makes the calling request its owner. When an
// older request is still running for this upload, often a request cut by the
// client that has not saved its last bytes yet, it waits for it, at most
// claimWait, then the older request cannot write anymore. The offset returned
// is then durable: the server must not answer an outdated offset, and the
// recommended way is to stop the older request
// (draft-ietf-httpbis-resumable-upload §10).
func ClaimUpload(fs VFS, id string) (*Upload, error) {
	owner := crand.Text()
	u, err := updateUpload(fs, id, func(fresh *Upload) error {
		if fresh.Completed {
			return nil
		}
		if fresh.Running {
			fresh.Previous = fresh.Owner
			fresh.Running = false
		}
		fresh.Owner = owner
		return nil
	})
	if err != nil || u.Completed || u.Previous == "" {
		return u, err
	}
	// The requests of this stack that wait for the same upload, like several
	// HEAD of a client, share one wait.
	waited, err, _ := claimWaits.Do(fs.DBPrefix()+"/"+id, func() (interface{}, error) {
		return waitPrevious(fs, id)
	})
	if err != nil {
		return nil, err
	}
	u = waited.(*Upload).Clone().(*Upload)
	u.Owner = owner
	return u, nil
}

var claimWaits singleflight.Group

// waitPrevious waits until the older request of an upload has saved its
// bytes, at most claimWait, then removes its right to write. It reads the
// upload more and more rarely.
func waitPrevious(fs VFS, id string) (*Upload, error) {
	deadline := time.Now().Add(claimWait)
	pause := claimWait / 100
	for {
		u, err := pollUpload(fs, id)
		if err != nil || u.Previous == "" {
			return u, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		time.Sleep(min(pause, left))
		pause = min(2*pause, claimWait/10)
	}
	return updateUpload(fs, id, func(fresh *Upload) error {
		fresh.Previous = ""
		return nil
	})
}

// start marks the request as running, so that a newer request waits for it
// to save the bytes it has received. The returned function marks its end.
func (u *Upload) start(fs VFS) (func(), error) {
	if err := u.update(fs, func(fresh *Upload) error {
		fresh.Running = true
		return nil
	}); err != nil {
		return nil, err
	}
	owner := u.Owner
	return func() {
		_, _ = updateUpload(fs, u.UploadID, func(fresh *Upload) error {
			switch {
			case fresh.Owner == owner && fresh.Running:
				fresh.Running = false
			case fresh.Previous == owner:
				fresh.Previous = ""
			default:
				return errNotSelected
			}
			return nil
		})
	}, nil
}

// reserveSlot gives a slot to the next write of the request, after checking
// that its end fits in the space left by the other uploads. The size of the
// write counts in the quota until the write is saved.
func (u *Upload) reserveSlot(fs VFS, end, size int64, tail bool) (int, error) {
	limit, err := UploadSpaceLimit(fs, u)
	if err != nil {
		return 0, err
	}
	if end > limit {
		return 0, deactivateUpload(fs, u, ErrFileTooBig)
	}
	_, err = u.updateAt(fs, u.Offset, func(fresh *Upload) error {
		if tail {
			fresh.NextTail++
		} else {
			fresh.NextPart++
		}
		fresh.Writing = size
		return nil
	})
	if tail {
		return u.NextTail, err
	}
	return u.NextPart, err
}

// updateAt applies change to the upload if nobody has written to it since the
// offset start. A request that a newer one is waiting for (see ClaimUpload)
// can still save the bytes it has received; lost tells it that it must stop
// after this write.
func (u *Upload) updateAt(fs VFS, start int64, change func(fresh *Upload) error) (bool, error) {
	owner := u.Owner
	lost := false
	saved, err := updateUpload(fs, u.UploadID, func(fresh *Upload) error {
		if fresh.Completed || fresh.Finalizing || fresh.Offset != start {
			return ErrUploadTakenOver
		}
		if fresh.Owner != owner && fresh.Previous != owner {
			return ErrUploadTakenOver
		}
		lost = fresh.Owner != owner
		return change(fresh)
	})
	if err != nil {
		return false, err
	}
	*u = *saved
	u.Owner = owner
	return lost, nil
}

// SetUploadLength records the length of the content, once it is known. Like
// at the creation, it must fit in the space left.
func SetUploadLength(fs VFS, u *Upload, length int64) error {
	limit, err := UploadSpaceLimit(fs, u)
	if err != nil {
		return err
	}
	if length > limit {
		return deactivateUpload(fs, u, ErrFileTooBig)
	}
	err = u.update(fs, func(fresh *Upload) error {
		if fresh.Length >= 0 && fresh.Length != length {
			return ErrUploadLength
		}
		if fresh.Offset > length {
			return ErrUploadLength
		}
		fresh.Length = length
		return nil
	})
	if errors.Is(err, ErrUploadLength) {
		return deactivateUpload(fs, u, err)
	}
	return err
}

// CancelUpload removes an upload and its content. A completed upload only
// loses its record: the file stays.
func CancelUpload(fs VFS, id string) error {
	return removeUpload(fs, id, func(u *Upload) bool { return !u.Removed && !u.expired() })
}

// CleanUploads removes the uploads that have expired, and finishes the removals
// that have failed. It returns the number of removed uploads.
func CleanUploads(fs VFS) (int, error) {
	return removeUploads(fs, func(u *Upload) bool { return u.Removed || u.expired() })
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
		err := removeUpload(fs, u.UploadID, selected)
		switch {
		case err == nil:
			removed++
		case errors.Is(err, errNotSelected), errors.Is(err, ErrUploadNotFound):
		default:
			errm = errors.Join(errm, err)
		}
	}
	return removed, errm
}

// removeUpload removes an upload in three steps: it is marked as removed, so
// that no request counts a new write, then its content is removed, then its
// document. A failure leaves the mark, and the cleaning runs it again.
func removeUpload(fs VFS, id string, selected func(u *Upload) bool) error {
	var u *Upload
	for i := 0; ; i++ {
		var err error
		u, err = getUploadDoc(fs, id)
		if err != nil {
			return err
		}
		if !selected(u) {
			if u.Removed || u.expired() {
				return ErrUploadNotFound
			}
			return errNotSelected
		}
		if u.Removed {
			break
		}
		u.Removed = true
		err = couchdb.UpdateDoc(fs, u)
		if err == nil {
			break
		}
		if !couchdb.IsConflictError(err) || i >= maxUpdateTries {
			return err
		}
	}
	committed, err := committedFile(fs, u)
	if err != nil {
		// The mark stays: the cleaning will finish the removal.
		return err
	}
	if !u.Completed && committed == nil {
		store, err := uploadStorage(fs)
		if err != nil {
			return err
		}
		if err := store.AbortUpload(u); err != nil {
			return err
		}
	}
	err = couchdb.DeleteDoc(fs, u)
	if couchdb.IsNotFoundError(err) {
		return nil
	}
	return err
}

// deactivateUpload removes an upload after an error that prevents resuming it,
// if the request is still its owner.
func deactivateUpload(fs VFS, u *Upload, cause error) error {
	err := removeUpload(fs, u.UploadID, func(fresh *Upload) bool {
		return !fresh.Removed && !fresh.Completed && fresh.Owner == u.Owner
	})
	if err == nil || errors.Is(err, ErrUploadNotFound) {
		// The upload does not exist anymore: the client must not resume it.
		return &RejectedUploadError{Err: cause}
	}
	if errors.Is(err, errNotSelected) {
		return ErrUploadTakenOver
	}
	return errors.Join(cause, err)
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
		if !u.active() || u.UploadID == exclude {
			continue
		}
		reserved := u.Offset + u.Writing
		if !u.ByLink {
			reserved = max(u.Length, reserved)
		}
		available -= reserved
		if available <= 0 {
			// Stop before the sum of the reservations overflows.
			return 0, nil
		}
	}
	return max(min(limit, available), 0), nil
}

// committedFile returns the file when the assembled content of the upload is
// already its content: the commit has succeeded, but the upload has not been
// marked as completed. Its content must not be aborted. Only a file that is
// not found tells that the content is not committed: on any other error, the
// caller must keep the content.
func committedFile(fs VFS, u *Upload) (*FileDoc, error) {
	if !u.Assembled || u.File == nil {
		return nil, nil
	}
	doc, err := fs.FileByID(u.File.ID())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if u.File.InternalID != "" {
		if doc.InternalID == u.File.InternalID {
			return doc, nil
		}
		// The file may have a newer content since: the one of the upload is
		// then a version.
		versions, err := VersionsFor(fs, doc.ID())
		if err != nil && !couchdb.IsNoDatabaseError(err) {
			return nil, err
		}
		for _, v := range versions {
			if v.DocID == doc.ID()+"/"+u.File.InternalID {
				return doc, nil
			}
		}
		return nil, nil
	}
	// The local file system has no internal ID: the content is known by its
	// size and md5.
	if doc.ByteSize != u.File.ByteSize || !bytes.Equal(doc.MD5Sum, u.File.MD5Sum) {
		return nil, nil
	}
	return doc, nil
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
	if isMissingIndex(err) {
		// The index of a database created by a stack that has stopped
		// before defining it.
		if err = couchdb.DefineIndex(fs, uploadsIndex); err == nil {
			err = couchdb.FindDocs(fs, consts.FilesUploads, req, &uploads)
		}
	}
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return nil, err
	}
	return uploads, nil
}

// isMissingIndex tells if CouchDB has no index for the query: cozy-stack
// returns no_index when CouchDB falls back to a full scan, and CouchDB returns
// no_usable_index when it refuses the query.
func isMissingIndex(err error) bool {
	var couchErr *couchdb.Error
	if !errors.As(err, &couchErr) {
		return false
	}
	return couchErr.Name == "no_index" || couchErr.Name == "no_usable_index"
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
