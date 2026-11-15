package vfs

import (
	"bytes"
	"crypto/md5"
	"encoding"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"time"

	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// AppendUpload writes the content of r to the upload. When complete is true,
// the upload is finished once r has been fully read: the file is added to the
// VFS and returned.
//
// The content is written to the storage part by part. When the request is
// interrupted, the bytes received so far are kept. The offset in CouchDB is
// updated only after the bytes have been written, so it never goes backward.
func AppendUpload(fs VFS, u *Upload, r io.Reader, complete bool) (*FileDoc, error) {
	store, err := uploadStorage(fs)
	if err != nil {
		return nil, err
	}
	partSize := uploadConfig().PartSize
	if maxsize := fs.MaxFileSize(); maxsize > 0 {
		// A Swift manifest accepts at most 1000 segments.
		partSize = max(partSize, maxsize/900)
	}
	limit, err := UploadSpaceLimit(fs, u)
	if err != nil {
		return nil, err
	}

	h := md5.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(u.MD5State); err != nil {
		return nil, err
	}

	buf, err := os.CreateTemp("", "cozy-upload-")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = buf.Close()
		_ = os.Remove(buf.Name())
	}()

	// The bytes after the last part go in front of the next part.
	var inBuf int64
	if u.Tail != nil {
		rc, err := store.OpenUploadTail(u)
		if err != nil {
			return nil, err
		}
		inBuf, err = io.Copy(buf, rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		if inBuf != u.Tail.Size {
			return nil, fmt.Errorf("upload %s: tail has %d bytes instead of %d", u.UploadID, inBuf, u.Tail.Size)
		}
	}

	var received int64 // bytes read from r and not yet durable
	var readErr error
	content := io.TeeReader(r, h)
	for {
		n, err := io.CopyN(buf, content, partSize-inBuf)
		inBuf += n
		received += n
		if u.Length >= 0 && u.Offset+received > u.Length {
			return nil, deactivateUpload(fs, u, ErrUploadLength)
		}
		if u.Offset+received > limit {
			return nil, deactivateUpload(fs, u, ErrFileTooBig)
		}
		if err == nil {
			err = u.locked(fs, func(fresh *Upload) error {
				return fresh.writeProgress(fs, store, received, h, func() error {
					return store.WriteUploadPart(fresh, io.NewSectionReader(buf, 0, inBuf), inBuf)
				})
			})
			if err != nil {
				return nil, err
			}
			received, inBuf = 0, 0
			if err := buf.Truncate(0); err != nil {
				return nil, err
			}
			if _, err := buf.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			continue
		}
		if !errors.Is(err, io.EOF) {
			readErr = err
		}
		break
	}

	if complete && readErr == nil {
		return finishUpload(fs, store, u, buf, inBuf, received, h)
	}

	if received > 0 {
		err := u.locked(fs, func(fresh *Upload) error {
			return fresh.writeProgress(fs, store, received, h, func() error {
				return store.WriteUploadTail(fresh, io.NewSectionReader(buf, 0, inBuf), inBuf)
			})
		})
		if err != nil {
			return nil, err
		}
	}
	return nil, readErr
}

func finishUpload(fs VFS, store UploadStorage, u *Upload, buf *os.File, inBuf, received int64, h hash.Hash) (*FileDoc, error) {
	var created *FileDoc
	err := u.locked(fs, func(fresh *Upload) error {
		if doc := committedFile(fs, fresh); doc != nil {
			created = doc
			return fresh.markCompleted(fs, doc)
		}
		total := fresh.Offset + received
		if fresh.Length >= 0 && total != fresh.Length {
			return deactivateLocked(fs, fresh, ErrUploadLength)
		}
		var olddoc *FileDoc
		if fresh.OldFileID != "" {
			var err error
			if olddoc, err = fs.FileByID(fresh.OldFileID); err != nil {
				return deactivateLocked(fs, fresh, err)
			}
			if fresh.OldRev != "" && olddoc.Rev() != fresh.OldRev {
				return deactivateLocked(fs, fresh, ErrConflict)
			}
		}
		newdoc := fresh.File.Clone().(*FileDoc)
		newdoc.ByteSize = total
		if olddoc != nil {
			// The file may have been moved, renamed, trashed or referenced
			// since the upload has started.
			newdoc.DocName = olddoc.DocName
			newdoc.DirID = olddoc.DirID
			newdoc.RestorePath = olddoc.RestorePath
			newdoc.Trashed = olddoc.Trashed
			newdoc.ReferencedBy = olddoc.ReferencedBy
		}
		// The storages close the readers that they receive.
		last := func() io.Reader { return io.NewSectionReader(buf, 0, inBuf) }

		// A content that fits in one part goes through the usual upload, so
		// that it is stored as a plain object.
		if len(fresh.Parts) == 0 && fresh.Offset == fresh.TailSize() {
			newdoc.InternalID = ""
			if len(newdoc.MD5Sum) == 0 {
				newdoc.MD5Sum = nil
			}
			if err := createFile(fs, newdoc, olddoc, last()); err != nil {
				return deactivateLocked(fs, fresh, err)
			}
			if err := store.AbortUpload(fresh); err != nil {
				return err
			}
			created = newdoc
			return fresh.markCompleted(fs, newdoc)
		}

		md5sum := h.Sum(nil)
		if len(newdoc.MD5Sum) > 0 && !bytes.Equal(newdoc.MD5Sum, md5sum) {
			return deactivateLocked(fs, fresh, ErrInvalidHash)
		}
		newdoc.MD5Sum = md5sum
		_, _, capsize, err := CheckAvailableDiskSpace(fs, newdoc)
		if err != nil {
			return deactivateLocked(fs, fresh, err)
		}

		if err := store.AssembleUpload(fresh, last(), inBuf); err != nil {
			return err
		}
		fresh.Assembled = true
		fresh.File = newdoc
		if err := couchdb.UpdateDoc(fs, fresh); err != nil {
			return err
		}

		if meta := NewMetaExtractor(newdoc); meta != nil {
			extractMetadata(store, fresh, newdoc, meta)
		}
		if err := store.CommitUpload(fresh, olddoc, capsize); err != nil {
			return deactivateLocked(fs, fresh, err)
		}
		created = newdoc
		return fresh.markCompleted(fs, newdoc)
	})
	return created, err
}

func createFile(fs VFS, newdoc, olddoc *FileDoc, content io.Reader) error {
	file, err := fs.CreateFile(newdoc, olddoc)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, content)
	if errc := file.Close(); err == nil {
		err = errc
	}
	return err
}

func extractMetadata(store UploadStorage, u *Upload, doc *FileDoc, meta *MetaExtractor) {
	rc, err := store.OpenUploadContent(u)
	if err != nil {
		(*meta).Abort(err)
		return
	}
	defer rc.Close()
	if _, err := io.Copy(*meta, rc); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		(*meta).Abort(err)
		return
	}
	if err := (*meta).Close(); err == nil {
		MergeMetadata(doc, (*meta).Result())
	}
}

// writeProgress writes the content with write, then saves the new offset and
// md5 state. A tail replaced by write is removed once the new state is saved.
func (u *Upload) writeProgress(fs VFS, store UploadStorage, received int64, h hash.Hash, write func() error) error {
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	replaced := u.Tail
	if err := write(); err != nil {
		return err
	}
	u.Offset += received
	u.MD5State = state
	u.ExpiresAt = time.Now().Add(uploadConfig().MaxAge)
	if err := couchdb.UpdateDoc(fs, u); err != nil {
		return err
	}
	if replaced != nil && (u.Tail == nil || u.Tail.Name != replaced.Name) {
		_ = store.DeleteUploadTail(u, replaced)
	}
	return nil
}

func (u *Upload) markCompleted(fs VFS, doc *FileDoc) error {
	u.Completed = true
	u.File = doc
	u.Offset = doc.ByteSize
	u.MD5State = nil
	u.Parts = nil
	u.Tail = nil
	u.ExpiresAt = time.Now().Add(uploadConfig().MaxAge)
	return couchdb.UpdateDoc(fs, u)
}
