package vfsafero

import (
	"io"
	"net/http"
	"os"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/spf13/afero"
)

const AvatarFilename = "avatar"

// NewAvatarFs creates a new avatar filesystem base on a afero.Fs.
func NewAvatarFs(fs afero.Fs) vfs.Avatarer {
	return &avatarFS{fs}
}

type avatarFS struct {
	fs afero.Fs
}

type avatarUpload struct {
	afero.File
	fs      afero.Fs
	tmpname string
}

func (u *avatarUpload) Close() error {
	if err := u.File.Close(); err != nil {
		_ = u.fs.Remove(u.tmpname)
		return err
	}
	return u.fs.Rename(u.tmpname, AvatarFilename)
}

func (a *avatarFS) CreateAvatar(contentType string) (io.WriteCloser, error) {
	if err := a.fs.MkdirAll("/", 0755); err != nil {
		return nil, err
	}
	f, err := afero.TempFile(a.fs, "/", AvatarFilename)
	if err != nil {
		return nil, err
	}
	tmpname := f.Name()
	u := &avatarUpload{
		File:    f,
		fs:      a.fs,
		tmpname: tmpname,
	}
	return u, nil
}

func (a *avatarFS) DeleteAvatar() error {
	if exists, err := a.AvatarExists(); err != nil {
		return err
	} else if !exists {
		return nil
	}
	if err := a.fs.Remove(AvatarFilename); err != nil {
		return err
	}
	return nil
}

func (a *avatarFS) AvatarExists() (bool, error) {
	infos, err := a.fs.Stat(AvatarFilename)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return infos.Size() > 0, nil
}

// OpenAvatar returns a reader over the stored avatar content and its
// content-type, or os.ErrNotExist if no avatar is stored. The content-type
// is not persisted on disk by this backend, so a generic value is returned.
func (a *avatarFS) OpenAvatar() (io.ReadCloser, string, error) {
	f, err := a.fs.Open(AvatarFilename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", os.ErrNotExist
		}
		return nil, "", err
	}
	return f, "application/octet-stream", nil
}

func (a *avatarFS) ServeAvatarContent(w http.ResponseWriter, req *http.Request) error {
	s, err := a.fs.Stat(AvatarFilename)
	if err != nil {
		if os.IsNotExist(err) {
			return os.ErrNotExist
		}
		return err
	}
	if s.Size() == 0 {
		return os.ErrInvalid
	}
	f, err := a.fs.Open(AvatarFilename)
	if err != nil {
		return err
	}
	defer f.Close()
	http.ServeContent(w, req, AvatarFilename, s.ModTime(), f)
	return nil
}
