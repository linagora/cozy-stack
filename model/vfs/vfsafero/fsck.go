package vfsafero

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/spf13/afero"
)

var errFailFast = errors.New("fail fast")

func (afs *aferoVFS) Fsck(accumulate func(log *vfs.FsckLog), failFast bool) error {
	entries := make(map[string]*vfs.TreeFile, 1024)
	tree, err := afs.BuildTree(func(f *vfs.TreeFile) {
		if !f.IsOrphan {
			entries[f.Fullpath] = f
		}
	})
	if err != nil {
		return err
	}
	if err = afs.CheckTreeIntegrity(tree, accumulate, failFast); err != nil {
		if errors.Is(err, vfs.ErrFsckFailFast) {
			return nil
		}
		return err
	}
	return afs.checkFiles(entries, accumulate, failFast)
}

func (afs *aferoVFS) CheckFilesConsistency(accumulate func(log *vfs.FsckLog), failFast bool) error {
	entries := make(map[string]*vfs.TreeFile, 1024)
	_, err := afs.BuildTree(func(f *vfs.TreeFile) {
		if !f.IsOrphan {
			entries[f.Fullpath] = f
		}
	})
	if err != nil {
		return err
	}
	return afs.checkFiles(entries, accumulate, failFast)
}

func (afs *aferoVFS) checkFiles(
	entries map[string]*vfs.TreeFile,
	accumulate func(log *vfs.FsckLog),
	failFast bool,
) error {
	versions := make(map[string]*vfs.Version, 1024)
	err := couchdb.ForeachDocs(afs, consts.FilesVersions, func(_ string, data json.RawMessage) error {
		v := &vfs.Version{}
		if erru := json.Unmarshal(data, v); erru != nil {
			return erru
		}
		versions[pathForVersion(v)] = v
		return nil
	})
	if err != nil {
		return err
	}

	err = afero.Walk(afs.fs, "/", func(fullpath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if fullpath == vfs.WebappsDirName ||
			fullpath == vfs.KonnectorsDirName ||
			fullpath == vfs.ThumbsDirName ||
			fullpath == vfs.UploadsDirName {
			return filepath.SkipDir
		}

		if strings.HasPrefix(fullpath, vfs.VersionsDirName) {
			if info.IsDir() {
				return nil
			}
			_, ok := versions[fullpath]
			if !ok {
				accumulate(&vfs.FsckLog{
					Type:       vfs.IndexMissing,
					IsVersion:  true,
					VersionDoc: fileInfosToVersionDoc(fullpath, info),
				})
			}
			delete(versions, fullpath)
			return nil
		}

		f, ok := entries[fullpath]
		if !ok {
			accumulate(&vfs.FsckLog{
				Type:    vfs.IndexMissing,
				IsFile:  true,
				FileDoc: fileInfosToFileDoc(fullpath, info),
			})
			if failFast {
				return errFailFast
			}
		} else if f.IsDir != info.IsDir() {
			if f.IsDir {
				accumulate(&vfs.FsckLog{
					Type:    vfs.TypeMismatch,
					IsFile:  true,
					FileDoc: f,
					DirDoc:  fileInfosToDirDoc(fullpath, info),
				})
			} else {
				accumulate(&vfs.FsckLog{
					Type:    vfs.TypeMismatch,
					IsFile:  false,
					DirDoc:  f,
					FileDoc: fileInfosToFileDoc(fullpath, info),
				})
			}
			if failFast {
				return errFailFast
			}
		} else if !f.IsDir {
			var fd afero.File
			fd, err = afs.fs.Open(fullpath)
			if err != nil {
				return err
			}
			h := md5.New()
			if _, err = io.Copy(h, fd); err != nil {
				fd.Close()
				return err
			}
			if err = fd.Close(); err != nil {
				return err
			}
			md5sum := h.Sum(nil)
			if !bytes.Equal(md5sum, f.MD5Sum) || f.ByteSize != info.Size() {
				accumulate(&vfs.FsckLog{
					Type:    vfs.ContentMismatch,
					IsFile:  true,
					FileDoc: f,
					ContentMismatch: &vfs.FsckContentMismatch{
						SizeFile:    info.Size(),
						SizeIndex:   f.ByteSize,
						MD5SumFile:  md5sum,
						MD5SumIndex: f.MD5Sum,
					},
				})
				if failFast {
					return errFailFast
				}
			}
		}
		delete(entries, fullpath)
		return nil
	})
	if err != nil {
		if errors.Is(err, errFailFast) {
			return nil
		}
		return err
	}

	for _, f := range entries {
		if f.IsDir {
			accumulate(&vfs.FsckLog{
				Type:   vfs.FSMissing,
				IsFile: false,
				DirDoc: f,
			})
		} else {
			accumulate(&vfs.FsckLog{
				Type:    vfs.FSMissing,
				IsFile:  true,
				FileDoc: f,
			})
		}
		if failFast {
			return nil
		}
	}

	for _, v := range versions {
		accumulate(&vfs.FsckLog{
			Type:       vfs.FSMissing,
			IsVersion:  true,
			VersionDoc: v,
		})
		if failFast {
			return nil
		}
	}

	return nil
}

func fileInfosToDirDoc(fullpath string, fileinfo os.FileInfo) *vfs.TreeFile {
	return &vfs.TreeFile{
		DirOrFileDoc: vfs.DirOrFileDoc{
			DirDoc: &vfs.DirDoc{
				Type:      consts.DirType,
				DocName:   fileinfo.Name(),
				DirID:     "",
				CreatedAt: fileinfo.ModTime(),
				UpdatedAt: fileinfo.ModTime(),
				Fullpath:  fullpath,
			},
		},
	}
}

func fileInfosToFileDoc(fullpath string, fileinfo os.FileInfo) *vfs.TreeFile {
	trashed := strings.HasPrefix(fullpath, vfs.TrashDirName)
	contentType, md5sum, _ := extractContentTypeAndMD5(fullpath)
	mime, class := vfs.ExtractMimeAndClass(contentType)
	return &vfs.TreeFile{
		DirOrFileDoc: vfs.DirOrFileDoc{
			DirDoc: &vfs.DirDoc{
				Type:      consts.FileType,
				DocName:   fileinfo.Name(),
				DirID:     "",
				CreatedAt: fileinfo.ModTime(),
				UpdatedAt: fileinfo.ModTime(),
				Fullpath:  fullpath,
			},
			ByteSize:   fileinfo.Size(),
			Mime:       mime,
			Class:      class,
			Executable: int(fileinfo.Mode()|0111) > 0,
			MD5Sum:     md5sum,
			Trashed:    trashed,
		},
	}
}

func fileInfosToVersionDoc(fullpath string, fileinfo os.FileInfo) *vfs.Version {
	_, md5sum, _ := extractContentTypeAndMD5(fullpath)
	v := &vfs.Version{
		UpdatedAt: fileinfo.ModTime(),
		ByteSize:  fileinfo.Size(),
		MD5Sum:    md5sum,
	}
	parts := strings.Split(fullpath, "/")
	var fileID string
	if len(parts) > 3 {
		fileID = parts[len(parts)-3] + parts[len(parts)-2]
	}
	v.DocID = fileID + "/" + parts[len(parts)-1]
	v.Rels.File.Data.ID = fileID
	v.Rels.File.Data.Type = consts.Files
	return v
}
