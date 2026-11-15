package vfss3

import (
	"fmt"
	"io"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/minio/minio-go/v7"
)

// The content of an upload is a multipart upload on the key of the file,
// started with the upload. The number of a part is its slot: two requests
// never write the same part, and the completion lists only the parts that
// count. A part must have at least 5 MiB, except the last one: the bytes
// received after the last part are kept in a temporary object (the tail), and
// go in front of the next part.

func (sfs *s3VFS) uploadKey(u *vfs.Upload) string {
	return MakeObjectKey(sfs.keyPrefix, u.File.DocID, u.File.InternalID)
}

func (sfs *s3VFS) uploadPrefix(u *vfs.Upload) string {
	return sfs.keyPrefix + vfs.UploadsObjectPrefix + u.UploadID + "/"
}

// StartUpload starts the multipart upload before the upload is saved, so that
// no two requests can start one each.
func (sfs *s3VFS) StartUpload(u *vfs.Upload) error {
	u.File.InternalID = NewInternalID()
	core := minio.Core{Client: sfs.client}
	id, err := core.NewMultipartUpload(sfs.ctx, sfs.bucket, sfs.uploadKey(u), minio.PutObjectOptions{
		ContentType: u.File.Mime,
	})
	if err != nil {
		return err
	}
	u.StorageID = id
	return nil
}

func (sfs *s3VFS) WriteUploadPart(u *vfs.Upload, slot int, content io.Reader, size int64) (vfs.UploadPart, error) {
	core := minio.Core{Client: sfs.client}
	part, err := core.PutObjectPart(sfs.ctx, sfs.bucket, sfs.uploadKey(u), u.StorageID, slot, content, size, minio.PutObjectPartOptions{})
	if err != nil {
		return vfs.UploadPart{}, err
	}
	return vfs.UploadPart{Number: slot, ETag: part.ETag, Size: size}, nil
}

func (sfs *s3VFS) WriteUploadTail(u *vfs.Upload, slot int, content io.Reader, size int64) (vfs.UploadPart, error) {
	key := fmt.Sprintf("%stail-%d", sfs.uploadPrefix(u), slot)
	if _, err := sfs.client.PutObject(sfs.ctx, sfs.bucket, key, content, size, minio.PutObjectOptions{}); err != nil {
		return vfs.UploadPart{}, err
	}
	return vfs.UploadPart{Number: slot, Name: key, Size: size}, nil
}

func (sfs *s3VFS) OpenUploadTail(u *vfs.Upload) (io.ReadCloser, error) {
	return sfs.client.GetObject(sfs.ctx, sfs.bucket, u.Tail.Name, minio.GetObjectOptions{})
}

// DeleteUploadPart removes a tail. A part of the multipart upload that does
// not count is left out of the completion, and dropped by S3 with it.
func (sfs *s3VFS) DeleteUploadPart(u *vfs.Upload, part vfs.UploadPart) error {
	if part.Name == "" {
		return nil
	}
	return sfs.client.RemoveObject(sfs.ctx, sfs.bucket, part.Name, minio.RemoveObjectOptions{})
}

func (sfs *s3VFS) AssembleUpload(u *vfs.Upload) error {
	parts := make([]minio.CompletePart, len(u.Parts))
	for i, p := range u.Parts {
		parts[i] = minio.CompletePart{PartNumber: p.Number, ETag: p.ETag}
	}
	core := minio.Core{Client: sfs.client}
	_, err := core.CompleteMultipartUpload(sfs.ctx, sfs.bucket, sfs.uploadKey(u), u.StorageID, parts, minio.PutObjectOptions{
		ContentType: u.File.Mime,
	})
	if minio.ToErrorResponse(err).Code == "NoSuchUpload" {
		// The stack has stopped after completing the multipart upload, before
		// recording it.
		info, errs := sfs.client.StatObject(sfs.ctx, sfs.bucket, sfs.uploadKey(u), minio.StatObjectOptions{})
		if errs == nil && info.Size == u.Offset {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	sfs.removeUploadObjects(u)
	return nil
}

func (sfs *s3VFS) OpenUploadContent(u *vfs.Upload) (io.ReadCloser, error) {
	return sfs.client.GetObject(sfs.ctx, sfs.bucket, sfs.uploadKey(u), minio.GetObjectOptions{})
}

func (sfs *s3VFS) CommitUpload(u *vfs.Upload, olddoc *vfs.FileDoc, capsize int64, check func() error) error {
	return sfs.commitFile(u.File, olddoc, u.File.ByteSize, capsize, false, check)
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
