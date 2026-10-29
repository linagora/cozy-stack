package office

import (
	"bytes"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
)

// EnsureFileForKey returns the file that will be written when OnlyOffice will
// save a document with the given key. The general case is that is the file
// that has been opened. But, it can also be a new file if a conflict has
// happened because a new version has been uploaded for this file (by the
// desktop client for example).
func EnsureFileForKey(inst *instance.Instance, key string) (*vfs.FileDoc, error) {
	detector, err := GetStore().GetDoc(inst, key)
	if err != nil {
		return nil, err
	}
	if detector == nil || detector.ID == "" || detector.Rev == "" {
		return nil, ErrInvalidKey
	}

	fs := inst.VFS()
	file, err := fs.FileByID(detector.ID)
	if err != nil {
		return nil, err
	}

	if file.Rev() == detector.Rev || bytes.Equal(file.MD5Sum, detector.MD5Sum) {
		return file, nil
	}

	// Manage the conflict
	conflictName := vfs.ConflictName(fs, file.DirID, file.DocName, true)
	newfile := vfs.CreateFileDocCopy(file, file.DirID, conflictName)
	newfile.CozyMetadata = vfs.NewCozyMetadata(inst.PageURL("/", nil))
	newfile.CozyMetadata.UpdatedAt = newfile.UpdatedAt
	newfile.CozyMetadata.UploadedAt = &newfile.UpdatedAt
	newfile.CozyMetadata.UploadedBy = &vfs.UploadedByEntry{Slug: OOSlug}
	if err := fs.CopyFile(file, newfile); err != nil {
		return nil, err
	}

	_ = GetStore().MoveToRevision(inst, key, newfile.ID(), newfile.Rev(), newfile.MD5Sum)
	return newfile, nil
}
