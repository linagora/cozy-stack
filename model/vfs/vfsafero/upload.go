package vfsafero

import (
	"fmt"
	"io"
	"os"
	"path"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// Each part and each tail of an upload is a file in the directory of the
// upload, under its own slot, so that two requests never write the same file.
// At the end, the parts are concatenated in a new file, moved at its place.

func uploadDir(u *vfs.Upload) string {
	return path.Join(vfs.UploadsDirName, u.UploadID)
}

func uploadContentPath(u *vfs.Upload) string {
	return path.Join(uploadDir(u), "content")
}

func (afs *aferoVFS) StartUpload(u *vfs.Upload) error {
	return afs.fs.MkdirAll(uploadDir(u), 0755)
}

func (afs *aferoVFS) writeUploadFile(name string, content io.Reader, size int64) error {
	f, err := afs.fs.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, content)
	if err == nil && n != size {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if errc := f.Close(); err == nil {
		err = errc
	}
	return err
}

func (afs *aferoVFS) WriteUploadPart(u *vfs.Upload, slot int, content io.Reader, size int64) (vfs.UploadPart, error) {
	name := path.Join(uploadDir(u), fmt.Sprintf("part-%d", slot))
	if err := afs.writeUploadFile(name, content, size); err != nil {
		return vfs.UploadPart{}, err
	}
	return vfs.UploadPart{Number: slot, Name: name, Size: size}, nil
}

func (afs *aferoVFS) WriteUploadTail(u *vfs.Upload, slot int, content io.Reader, size int64) (vfs.UploadPart, error) {
	name := path.Join(uploadDir(u), fmt.Sprintf("tail-%d", slot))
	if err := afs.writeUploadFile(name, content, size); err != nil {
		return vfs.UploadPart{}, err
	}
	return vfs.UploadPart{Number: slot, Name: name, Size: size}, nil
}

func (afs *aferoVFS) OpenUploadTail(u *vfs.Upload) (io.ReadCloser, error) {
	return afs.fs.Open(u.Tail.Name)
}

func (afs *aferoVFS) DeleteUploadPart(u *vfs.Upload, part vfs.UploadPart) error {
	err := afs.fs.Remove(part.Name)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// AssembleUpload concatenates the parts in a new file, and renames it: a file
// with the right size is the result of a previous call.
func (afs *aferoVFS) AssembleUpload(u *vfs.Upload) error {
	if info, err := afs.fs.Stat(uploadContentPath(u)); err == nil && info.Size() == u.Offset {
		return nil
	}
	tmp := path.Join(uploadDir(u), "content-"+utils.RandomString(16))
	f, err := afs.fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	for _, p := range u.Parts {
		var part io.ReadCloser
		part, err = afs.fs.Open(p.Name)
		if err != nil {
			break
		}
		_, err = io.Copy(f, part)
		_ = part.Close()
		if err != nil {
			break
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if errc := f.Close(); err == nil {
		err = errc
	}
	if err == nil {
		err = afs.fs.Rename(tmp, uploadContentPath(u))
	}
	if err != nil {
		_ = afs.fs.Remove(tmp)
	}
	return err
}

func (afs *aferoVFS) OpenUploadContent(u *vfs.Upload) (io.ReadCloser, error) {
	return afs.fs.Open(uploadContentPath(u))
}

func (afs *aferoVFS) CommitUpload(u *vfs.Upload, olddoc *vfs.FileDoc, capsize int64, check func() error) error {
	if err := afs.commitFile(u.File, olddoc, uploadContentPath(u), u.File.ByteSize, capsize, check); err != nil {
		return err
	}
	_ = afs.fs.RemoveAll(uploadDir(u))
	return nil
}

func (afs *aferoVFS) AbortUpload(u *vfs.Upload) error {
	return afs.fs.RemoveAll(uploadDir(u))
}

var _ vfs.UploadStorage = (*aferoVFS)(nil)
