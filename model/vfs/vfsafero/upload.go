package vfsafero

import (
	"errors"
	"io"
	"os"
	"path"

	"github.com/cozy/cozy-stack/model/vfs"
)

// The content of an upload is written in a single file, part after part, and
// moved to its place at the end.

func uploadPath(u *vfs.Upload) string {
	return path.Join(vfs.UploadsDirName, u.UploadID)
}

func (afs *aferoVFS) StartUpload(u *vfs.Upload) error {
	if err := afs.fs.MkdirAll(vfs.UploadsDirName, 0755); err != nil {
		return err
	}
	f, err := afs.fs.OpenFile(uploadPath(u), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	return f.Close()
}

// writeAt writes the content at the offset of the upload. The bytes after the
// offset are left by an interrupted write and are dropped. The file is opened
// for each write so that no handle outlives the lock of the upload.
func (afs *aferoVFS) writeAt(u *vfs.Upload, content io.Reader, size int64) error {
	f, err := afs.fs.OpenFile(uploadPath(u), os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if err = f.Truncate(u.Offset); err == nil {
		if _, err = f.Seek(u.Offset, io.SeekStart); err == nil {
			var n int64
			n, err = io.Copy(f, content)
			if err == nil && n != size {
				err = io.ErrShortWrite
			}
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if errc := f.Close(); err == nil {
		err = errc
	}
	return err
}

func (afs *aferoVFS) WriteUploadPart(u *vfs.Upload, content io.Reader, size int64) error {
	if err := afs.writeAt(u, content, size); err != nil {
		return err
	}
	u.Parts = append(u.Parts, vfs.UploadPart{Size: size})
	return nil
}

// WriteUploadTail is the same as WriteUploadPart: a file accepts parts of any
// size.
func (afs *aferoVFS) WriteUploadTail(u *vfs.Upload, content io.Reader, size int64) error {
	return afs.WriteUploadPart(u, content, size)
}

func (afs *aferoVFS) OpenUploadTail(u *vfs.Upload) (io.ReadCloser, error) {
	return nil, errors.New("no tail for the uploads on a local file system")
}

func (afs *aferoVFS) DeleteUploadTail(u *vfs.Upload, tail *vfs.UploadPart) error {
	return nil
}

// AssembleUpload drops the bytes after the final size, left by a write
// interrupted before the end of the upload was recorded.
func (afs *aferoVFS) AssembleUpload(u *vfs.Upload) error {
	f, err := afs.fs.OpenFile(uploadPath(u), os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	err = f.Truncate(u.Offset)
	if errc := f.Close(); err == nil {
		err = errc
	}
	return err
}

func (afs *aferoVFS) OpenUploadContent(u *vfs.Upload) (io.ReadCloser, error) {
	return afs.fs.Open(uploadPath(u))
}

func (afs *aferoVFS) CommitUpload(u *vfs.Upload, olddoc *vfs.FileDoc, capsize int64) error {
	return afs.commitFile(u.File, olddoc, uploadPath(u), u.File.ByteSize, capsize)
}

func (afs *aferoVFS) AbortUpload(u *vfs.Upload) error {
	err := afs.fs.Remove(uploadPath(u))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

var _ vfs.UploadStorage = (*aferoVFS)(nil)
