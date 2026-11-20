package vfs

import (
	"errors"
	"io"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

var (
	// ErrUploadNotFound is used when an upload has expired, has been cancelled
	// or has never existed.
	ErrUploadNotFound = errors.New("upload not found")
	// ErrUploadTakenOver is used when a newer request has resumed the upload:
	// the older one must stop writing (draft-ietf-httpbis-resumable-upload §10).
	ErrUploadTakenOver = errors.New("upload resumed by another request")
	// ErrUploadLength is used when more bytes than the announced length are
	// sent, or when the announced lengths differ.
	ErrUploadLength = errors.New("upload length exceeded or inconsistent")
	// ErrTooManyUploads is used when an instance has reached the maximal
	// number of uploads in progress.
	ErrTooManyUploads = errors.New("too many uploads in progress")
)

// UploadPart is a piece of the content of an upload, written to the storage.
type UploadPart struct {
	Number int    `json:"number,omitempty"`
	Name   string `json:"name,omitempty"`
	ETag   string `json:"etag,omitempty"`
	Size   int64  `json:"size"`
}

// Upload is the state of a resumable upload, as defined by
// draft-ietf-httpbis-resumable-upload. It is persisted in CouchDB so that any
// stack can resume it, even after a restart.
type Upload struct {
	UploadID  string `json:"_id,omitempty"`
	UploadRev string `json:"_rev,omitempty"`

	// File is the document of the file to create, with its final DocID and
	// InternalID.
	File      *FileDoc `json:"file"`
	OldFileID string   `json:"old_file_id,omitempty"`
	OldRev    string   `json:"old_rev,omitempty"`
	// DriveID and DriveMember are the shared drive and its member who have
	// created the upload, when it comes from a member: only this member can
	// resume it, and only under this drive.
	DriveID     string `json:"drive_id,omitempty"`
	DriveMember string `json:"drive_member,omitempty"`
	// Length is the announced size of the content, or -1 while unknown.
	Length int64 `json:"length"`
	// Offset is the number of bytes durably written to the storage.
	Offset   int64  `json:"offset"`
	MD5State []byte `json:"md5_state,omitempty"`
	// Owner is the token of the last request that has created or resumed the
	// upload. Only this request can write, apart from Previous.
	Owner string `json:"owner,omitempty"`
	// Running is set while the request of the owner appends to the upload.
	Running bool `json:"running,omitempty"`
	// Previous is the token of an older request still running when a newer
	// one has taken the upload: it can save the bytes it has received until
	// the newer one stops waiting for it.
	Previous string `json:"previous_owner,omitempty"`
	// ByLink is set for an upload made through a sharing by link: its length
	// does not reserve space in the quota, only its written bytes count.
	ByLink bool `json:"by_link,omitempty"`

	Parts []UploadPart `json:"parts,omitempty"`
	// Tail holds the bytes after the last part, when the storage cannot keep
	// parts smaller than the part size (S3, and Swift to bound the number of
	// segments).
	Tail      *UploadPart `json:"tail,omitempty"`
	StorageID string      `json:"storage_id,omitempty"`
	// NextPart and NextTail are the last slots given to a part and to a tail.
	// Each write gets its own slot, so two requests never write the same
	// object. The tails have their own slots: S3 accepts at most 10,000 part
	// numbers.
	NextPart int `json:"next_part,omitempty"`
	NextTail int `json:"next_tail,omitempty"`
	// Writing is the size of the write in progress, counted in the quota by
	// the other uploads.
	Writing int64 `json:"writing,omitempty"`
	// Finalizing is set once all the content is written: the offset is the
	// final size, and the next steps (assembling, indexing) can be run again
	// after a stop of the stack.
	Finalizing bool `json:"finalizing,omitempty"`
	Assembled  bool `json:"assembled,omitempty"`
	Completed  bool `json:"completed"`
	// Removed is set before the content of the upload is removed: the upload
	// is then not found, and a removal that fails is run again by the cleaning.
	Removed bool `json:"removed,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ID returns the upload identifier
func (u *Upload) ID() string { return u.UploadID }

// Rev returns the upload revision
func (u *Upload) Rev() string { return u.UploadRev }

// DocType returns the upload document type
func (u *Upload) DocType() string { return consts.FilesUploads }

// SetID changes the upload identifier
func (u *Upload) SetID(id string) { u.UploadID = id }

// SetRev changes the upload revision
func (u *Upload) SetRev(rev string) { u.UploadRev = rev }

// Clone implements couchdb.Doc
func (u *Upload) Clone() couchdb.Doc {
	cloned := *u
	if u.File != nil {
		cloned.File = u.File.Clone().(*FileDoc)
	}
	cloned.MD5State = append([]byte(nil), u.MD5State...)
	cloned.Parts = append([]UploadPart(nil), u.Parts...)
	if u.Tail != nil {
		tail := *u.Tail
		cloned.Tail = &tail
	}
	return &cloned
}

// TailSize returns the number of bytes kept after the last part.
func (u *Upload) TailSize() int64 {
	if u.Tail == nil {
		return 0
	}
	return u.Tail.Size
}

func (u *Upload) expired() bool {
	return time.Now().After(u.ExpiresAt)
}

// UploadStorage is implemented by the file systems that can keep the content
// of an upload across several requests.
type UploadStorage interface {
	// StartUpload prepares the storage for a new upload, and sets the
	// InternalID of the file.
	StartUpload(u *Upload) error
	// WriteUploadPart writes the content that follows the last part, under the
	// given slot. It does not change u: the part counts only once the state of
	// the upload has been saved with it.
	WriteUploadPart(u *Upload, slot int, content io.Reader, size int64) (UploadPart, error)
	// WriteUploadTail writes the content that follows the last part, when it is
	// smaller than a part, under the given slot.
	WriteUploadTail(u *Upload, slot int, content io.Reader, size int64) (UploadPart, error)
	// OpenUploadTail returns the content of the tail.
	OpenUploadTail(u *Upload) (io.ReadCloser, error)
	// DeleteUploadPart removes a part or a tail that does not count: a
	// replaced tail, or the write of a request that has lost the upload.
	DeleteUploadPart(u *Upload, part UploadPart) error
	// AssembleUpload puts the parts together at the place of the file. It can
	// be called again after a stop of the stack.
	AssembleUpload(u *Upload) error
	// OpenUploadContent returns the assembled content.
	OpenUploadContent(u *Upload) (io.ReadCloser, error)
	// CommitUpload adds the assembled file to the index, like the Close method
	// of a file created with CreateFile. check runs once the lock of the VFS
	// is held, and stops the commit when it fails.
	CommitUpload(u *Upload, olddoc *FileDoc, capsize int64, check func() error) error
	// AbortUpload removes the content of an upload that is not completed.
	AbortUpload(u *Upload) error
}

func uploadConfig() config.FsResumableUploads {
	cfg := config.GetConfig().Fs.ResumableUploads
	if cfg.PartSize <= 0 {
		cfg.PartSize = 16 << 20
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 7 * 24 * time.Hour
	}
	if cfg.MaxPerAccount <= 0 {
		cfg.MaxPerAccount = 20
	}
	if cfg.MaxPerInstance <= 0 {
		cfg.MaxPerInstance = 200
	}
	return cfg
}

// UploadPartSize returns the size of the parts written to the storage.
func UploadPartSize() int64 {
	return uploadConfig().PartSize
}

func uploadStorage(fs VFS) (UploadStorage, error) {
	store, ok := fs.(UploadStorage)
	if !ok {
		return nil, errors.New("this file system does not support resumable uploads")
	}
	return store, nil
}

// SupportsUploads returns true if the file system can keep an upload across
// several requests.
func SupportsUploads(fs VFS) bool {
	_, ok := fs.(UploadStorage)
	return ok
}

var _ couchdb.Doc = &Upload{}
