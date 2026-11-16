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
)

// AppendUpload writes the content of r to the upload. When complete is true,
// the upload is finished once r has been fully read: the file is added to the
// VFS and returned.
//
// The content is written to the storage part by part, without lock: each
// write has its own slot, and counts only once the state of the upload has
// been saved with it. When the request is interrupted, the bytes received so
// far are kept. The offset in CouchDB is updated only after the bytes have
// been written, so it never goes backward.
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
	stop, err := u.start(fs)
	if err != nil {
		return nil, err
	}
	defer stop()

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
			err = u.writeProgress(fs, store, received, h, false, io.NewSectionReader(buf, 0, inBuf), inBuf)
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

	if err := uploadStep("received"); err != nil {
		return nil, err
	}
	if received > 0 {
		err := u.writeProgress(fs, store, received, h, true, io.NewSectionReader(buf, 0, inBuf), inBuf)
		if err != nil {
			return nil, err
		}
	}
	return nil, readErr
}

// writeProgress writes the bytes after the last part, as a part or as the
// tail, then saves the new offset and md5 state, if the request is still the
// owner of the upload and nobody has written in the meantime. A write that
// does not count, and a replaced tail, are removed.
func (u *Upload) writeProgress(fs VFS, store UploadStorage, received int64, h hash.Hash, tail bool, content io.Reader, size int64) error {
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	start := u.Offset
	slot, err := u.reserveSlot(fs, start+received, received, tail)
	if err != nil {
		return err
	}
	if err := uploadStep("write"); err != nil {
		return err
	}
	var part UploadPart
	if tail {
		part, err = store.WriteUploadTail(u, slot, content, size)
	} else {
		part, err = store.WriteUploadPart(u, slot, content, size)
	}
	if err != nil {
		return err
	}
	if err := uploadStep("written"); err != nil {
		return err
	}
	replaced := u.Tail
	written := *u
	owner := u.Owner
	lost, err := u.updateAt(fs, start, func(fresh *Upload) error {
		if fresh.Owner != owner {
			// The last write of an older request: the newer one stops
			// waiting for it.
			fresh.Previous = ""
		}
		if tail {
			fresh.Tail = &part
		} else {
			fresh.Parts = append(fresh.Parts, part)
			fresh.Tail = nil
		}
		fresh.Offset += received
		fresh.Writing = 0
		fresh.MD5State = state
		fresh.ExpiresAt = time.Now().Add(uploadConfig().MaxAge)
		return nil
	})
	if err != nil {
		_ = store.DeleteUploadPart(&written, part)
		return err
	}
	if replaced != nil {
		_ = store.DeleteUploadPart(u, *replaced)
	}
	if lost {
		return ErrUploadTakenOver
	}
	return nil
}

func finishUpload(fs VFS, store UploadStorage, u *Upload, buf *os.File, inBuf, received int64, h hash.Hash) (*FileDoc, error) {
	if doc, err := committedFile(fs, u); err != nil || doc != nil {
		if err != nil {
			return nil, err
		}
		return doc, u.markCompleted(fs, doc)
	}
	total := u.Offset + received
	if u.Length >= 0 && total != u.Length {
		return nil, finishError(fs, u, ErrUploadLength)
	}
	if u.Finalizing && received > 0 {
		return nil, finishError(fs, u, ErrUploadLength)
	}
	var olddoc *FileDoc
	if u.OldFileID != "" {
		var err error
		if olddoc, err = fs.FileByID(u.OldFileID); err != nil {
			return nil, finishError(fs, u, err)
		}
		if u.OldRev != "" && olddoc.Rev() != u.OldRev {
			return nil, finishError(fs, u, ErrConflict)
		}
	}
	newdoc := u.File.Clone().(*FileDoc)
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
	md5sum := h.Sum(nil)

	// A content that fits in one part goes through the usual upload, so that
	// it is stored as a plain object.
	if !u.Finalizing && len(u.Parts) == 0 && u.Offset == u.TailSize() {
		if err := ownsUpload(fs, u); err != nil {
			return nil, err
		}
		newdoc.InternalID = ""
		if len(newdoc.MD5Sum) == 0 {
			newdoc.MD5Sum = nil
		}
		if err := createFile(fs, newdoc, olddoc, last()); err != nil {
			if doc := createdFile(fs, u, total, md5sum); doc != nil {
				return doc, u.markCompleted(fs, doc)
			}
			return nil, finishError(fs, u, err)
		}
		_ = store.AbortUpload(u)
		return newdoc, u.markCompleted(fs, newdoc)
	}

	if !u.Finalizing {
		if len(newdoc.MD5Sum) > 0 && !bytes.Equal(newdoc.MD5Sum, md5sum) {
			return nil, finishError(fs, u, ErrInvalidHash)
		}
		newdoc.MD5Sum = md5sum
		if _, _, _, err := CheckAvailableDiskSpace(fs, newdoc); err != nil {
			return nil, finishError(fs, u, err)
		}
		// The offset becomes the final size before anything is assembled:
		// after a stop of the stack, the client sends an empty request to
		// finish, and no byte is written twice.
		if err := u.finalize(fs, store, received, h, last(), inBuf, md5sum); err != nil {
			return nil, err
		}
		if err := uploadStep("finalizing"); err != nil {
			return nil, err
		}
	}
	_, _, capsize, err := CheckAvailableDiskSpace(fs, newdoc)
	if err != nil {
		return nil, finishError(fs, u, err)
	}

	// The assembling can run again: it gives the same object.
	if !u.Assembled {
		if err := store.AssembleUpload(u); err != nil {
			return nil, err
		}
		if err := u.update(fs, func(fresh *Upload) error {
			fresh.Assembled = true
			return nil
		}); err != nil {
			return nil, err
		}
		if err := uploadStep("assembled"); err != nil {
			return nil, err
		}
	}

	committing := *u
	committing.File = newdoc
	if meta := NewMetaExtractor(newdoc); meta != nil {
		extractMetadata(store, &committing, newdoc, meta)
	}
	if err := ownsUpload(fs, u); err != nil {
		return nil, err
	}
	err = uploadStep("commit")
	if err == nil {
		// The commit takes the lock of the VFS, like the usual upload. The
		// upload may have been removed while the request was waiting for it,
		// by a storage migration for example.
		err = store.CommitUpload(&committing, olddoc, capsize, func() error {
			if err := ownsUpload(fs, u); err != nil {
				return err
			}
			return uploadStep("locked")
		})
	}
	if errors.Is(err, ErrUploadNotFound) || errors.Is(err, ErrUploadTakenOver) {
		return nil, err
	}
	if err != nil {
		// Another request may have indexed the file in the meantime. When
		// this is unknown, the upload stays resumable.
		doc, errc := committedFile(fs, u)
		if errc != nil {
			return nil, errc
		}
		if doc != nil {
			return doc, u.markCompleted(fs, doc)
		}
		return nil, finishError(fs, u, err)
	}
	if err := uploadStep("committed"); err != nil {
		return nil, err
	}
	return newdoc, u.markCompleted(fs, newdoc)
}

// finalize writes the last bytes and saves the final size and md5.
func (u *Upload) finalize(fs VFS, store UploadStorage, received int64, h hash.Hash, last io.Reader, size int64, md5sum []byte) error {
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	start := u.Offset
	var part *UploadPart
	if size > 0 || len(u.Parts) == 0 {
		slot, err := u.reserveSlot(fs, start+received, received, false)
		if err != nil {
			return err
		}
		written, err := store.WriteUploadPart(u, slot, last, size)
		if err != nil {
			return err
		}
		part = &written
	}
	replaced := u.Tail
	before := *u
	err = u.update(fs, func(fresh *Upload) error {
		if fresh.Offset != start || fresh.Finalizing {
			return ErrUploadTakenOver
		}
		if part != nil {
			fresh.Parts = append(fresh.Parts, *part)
		}
		fresh.Tail = nil
		fresh.Offset += received
		fresh.Writing = 0
		fresh.Length = fresh.Offset
		fresh.MD5State = state
		fresh.Finalizing = true
		fresh.File.MD5Sum = md5sum
		fresh.File.ByteSize = fresh.Offset
		return nil
	})
	if err != nil {
		if part != nil {
			_ = store.DeleteUploadPart(&before, *part)
		}
		return err
	}
	if replaced != nil {
		_ = store.DeleteUploadPart(u, *replaced)
	}
	return nil
}

// ownsUpload checks that the request is still the owner of the upload, before
// a step that is not guarded by the state of the upload.
func ownsUpload(fs VFS, u *Upload) error {
	fresh, err := GetUpload(fs, u.UploadID)
	if err != nil {
		return err
	}
	if fresh.Completed || fresh.Owner != u.Owner {
		return ErrUploadTakenOver
	}
	return nil
}

// createdFile returns the file created by the usual upload of a small content
// when another request has created it first.
func createdFile(fs VFS, u *Upload, size int64, md5sum []byte) *FileDoc {
	doc, err := fs.FileByID(u.File.ID())
	if err != nil || doc.ByteSize != size || !bytes.Equal(doc.MD5Sum, md5sum) {
		return nil
	}
	return doc
}

// RejectedUploadError is the error of an upload that has been removed because
// it cannot become a file (too big, wrong length, md5 or name). It is the
// final answer for the upload, unlike the other errors, after which the client
// can resume it.
type RejectedUploadError struct {
	Err error
}

func (e *RejectedUploadError) Error() string { return e.Err.Error() }
func (e *RejectedUploadError) Unwrap() error { return e.Err }

// finishError removes the upload when the error tells that the file cannot be
// created, and keeps it resumable for a passing error: the client finishes it
// later with an empty request.
func finishError(fs VFS, u *Upload, err error) error {
	for _, final := range []error{
		os.ErrExist, os.ErrNotExist, ErrConflict, ErrFileTooBig, ErrMaxFileSize,
		ErrParentDoesNotExist, ErrParentInTrash, ErrIllegalFilename,
		ErrInvalidHash, ErrContentLengthMismatch, ErrUploadLength,
	} {
		if errors.Is(err, final) {
			return deactivateUpload(fs, u, err)
		}
	}
	return err
}

// uploadStepHook is called after each step of the end of an upload. The tests
// use it to stop there, as a stop of the stack would.
var uploadStepHook func(step string) error

func uploadStep(step string) error {
	if uploadStepHook == nil {
		return nil
	}
	return uploadStepHook(step)
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

// markCompleted records that the file is indexed. The upload is completed even
// if another request has resumed it in the meantime.
func (u *Upload) markCompleted(fs VFS, doc *FileDoc) error {
	saved, err := updateUpload(fs, u.UploadID, func(fresh *Upload) error {
		fresh.Completed = true
		fresh.File = doc
		fresh.Offset = doc.ByteSize
		fresh.MD5State = nil
		fresh.Parts = nil
		fresh.Tail = nil
		fresh.ExpiresAt = time.Now().Add(uploadConfig().MaxAge)
		return nil
	})
	if errors.Is(err, ErrUploadNotFound) {
		// The upload has been cancelled after the file was indexed.
		return nil
	}
	if err != nil {
		return err
	}
	*u = *saved
	return nil
}
