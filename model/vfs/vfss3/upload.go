package vfss3

import (
	"fmt"
	"io"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/minio/minio-go/v7"
)

// The content of an upload is a multipart upload on the key of the file. A
// part must have at least 5 MiB, except the last one: the bytes received after
// the last part are kept in a temporary object (the tail), and go in front of
// the next part.

func (sfs *s3VFS) uploadKey(u *vfs.Upload) string {
	return MakeObjectKey(sfs.keyPrefix, u.File.DocID, u.File.InternalID)
}

func (sfs *s3VFS) uploadPrefix(u *vfs.Upload) string {
	return sfs.keyPrefix + vfs.UploadsObjectPrefix + u.UploadID + "/"
}

func (sfs *s3VFS) StartUpload(u *vfs.Upload) error {
	u.File.InternalID = NewInternalID()
	return nil
}

func (sfs *s3VFS) writePart(u *vfs.Upload, content io.Reader, size int64) error {
	core := minio.Core{Client: sfs.client}
	started := false
	if u.StorageID == "" {
		id, err := core.NewMultipartUpload(sfs.ctx, sfs.bucket, sfs.uploadKey(u), minio.PutObjectOptions{
			ContentType: u.File.Mime,
		})
		if err != nil {
			return err
		}
		u.StorageID = id
		started = true
	}
	number := len(u.Parts) + 1
	part, err := core.PutObjectPart(sfs.ctx, sfs.bucket, sfs.uploadKey(u), u.StorageID, number, content, size, minio.PutObjectPartOptions{})
	if err != nil {
		if started {
			// The ID is saved only with a part: nothing else would abort it.
			_ = core.AbortMultipartUpload(sfs.ctx, sfs.bucket, sfs.uploadKey(u), u.StorageID)
			u.StorageID = ""
		}
		return err
	}
	u.Parts = append(u.Parts, vfs.UploadPart{Number: number, ETag: part.ETag, Size: size})
	u.Tail = nil
	return nil
}

func (sfs *s3VFS) WriteUploadPart(u *vfs.Upload, content io.Reader, size int64) error {
	return sfs.writePart(u, content, size)
}

func (sfs *s3VFS) WriteUploadTail(u *vfs.Upload, content io.Reader, size int64) error {
	end := u.Offset - u.TailSize() + size
	// The name depends on the end offset: a tail is never overwritten by a
	// different content.
	key := fmt.Sprintf("%stail-%d", sfs.uploadPrefix(u), end)
	if _, err := sfs.client.PutObject(sfs.ctx, sfs.bucket, key, content, size, minio.PutObjectOptions{}); err != nil {
		return err
	}
	u.Tail = &vfs.UploadPart{Name: key, Size: size}
	return nil
}

func (sfs *s3VFS) OpenUploadTail(u *vfs.Upload) (io.ReadCloser, error) {
	return sfs.client.GetObject(sfs.ctx, sfs.bucket, u.Tail.Name, minio.GetObjectOptions{})
}

func (sfs *s3VFS) DeleteUploadTail(u *vfs.Upload, tail *vfs.UploadPart) error {
	return sfs.client.RemoveObject(sfs.ctx, sfs.bucket, tail.Name, minio.RemoveObjectOptions{})
}

func (sfs *s3VFS) AssembleUpload(u *vfs.Upload, last io.Reader, size int64) error {
	if size > 0 || len(u.Parts) == 0 {
		if err := sfs.writePart(u, last, size); err != nil {
			return err
		}
	}
	parts := make([]minio.CompletePart, len(u.Parts))
	for i, p := range u.Parts {
		parts[i] = minio.CompletePart{PartNumber: p.Number, ETag: p.ETag}
	}
	core := minio.Core{Client: sfs.client}
	_, err := core.CompleteMultipartUpload(sfs.ctx, sfs.bucket, sfs.uploadKey(u), u.StorageID, parts, minio.PutObjectOptions{
		ContentType: u.File.Mime,
	})
	if err != nil {
		return err
	}
	sfs.removeUploadObjects(u)
	return nil
}

func (sfs *s3VFS) OpenUploadContent(u *vfs.Upload) (io.ReadCloser, error) {
	return sfs.client.GetObject(sfs.ctx, sfs.bucket, sfs.uploadKey(u), minio.GetObjectOptions{})
}

func (sfs *s3VFS) CommitUpload(u *vfs.Upload, olddoc *vfs.FileDoc, capsize int64) error {
	return sfs.commitFile(u.File, olddoc, u.File.ByteSize, capsize, false)
}

func (sfs *s3VFS) AbortUpload(u *vfs.Upload) error {
	assembled := u.Assembled
	if !assembled && u.StorageID != "" {
		core := minio.Core{Client: sfs.client}
		err := core.AbortMultipartUpload(sfs.ctx, sfs.bucket, sfs.uploadKey(u), u.StorageID)
		if err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			return err
		}
		// The multipart upload may have been completed without being recorded.
		assembled = err != nil
	}
	if assembled {
		if err := sfs.client.RemoveObject(sfs.ctx, sfs.bucket, sfs.uploadKey(u), minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	sfs.removeUploadObjects(u)
	return nil
}

// removeUploadObjects removes the tails of an upload.
func (sfs *s3VFS) removeUploadObjects(u *vfs.Upload) {
	objects := sfs.client.ListObjects(sfs.ctx, sfs.bucket, minio.ListObjectsOptions{
		Prefix:    sfs.uploadPrefix(u),
		Recursive: true,
	})
	for obj := range objects {
		if obj.Err != nil {
			sfs.log.Warnf("Cannot list the objects of upload %s: %s", u.UploadID, obj.Err)
			return
		}
		if err := sfs.client.RemoveObject(sfs.ctx, sfs.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			sfs.log.Warnf("Cannot remove %s: %s", obj.Key, err)
		}
	}
}

var _ vfs.UploadStorage = (*s3VFS)(nil)
