package vfss3

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/s3util"
	"github.com/labstack/echo/v4"
	"github.com/minio/minio-go/v7"
)

var unixEpochZero = time.Time{}

// NewThumbsFs creates a new thumbnail filesystem backed by S3.
func NewThumbsFs(client *minio.Client, bucket, keyPrefix string) vfs.Thumbser {
	return &thumbsS3{
		client:    client,
		bucket:    bucket,
		keyPrefix: keyPrefix,
		ctx:       context.Background(),
	}
}

type thumbsS3 struct {
	client    *minio.Client
	bucket    string
	keyPrefix string
	ctx       context.Context
}

type s3Thumb struct {
	pw     *io.PipeWriter
	errCh  chan error
	client *minio.Client
	bucket string
	name   string
	ctx    context.Context
}

func (t *s3Thumb) Write(p []byte) (int, error) {
	return t.pw.Write(p)
}

func (t *s3Thumb) Commit() error {
	if err := t.pw.Close(); err != nil {
		return err
	}
	return <-t.errCh
}

func (t *s3Thumb) Abort() error {
	// Close the pipe with an error to cancel the PutObject goroutine.
	errc := t.pw.CloseWithError(fmt.Errorf("thumbnail creation aborted"))
	// Drain the errCh so the goroutine is not leaked.
	<-t.errCh
	// Try to remove the possibly partially written object.
	errd := t.client.RemoveObject(t.ctx, t.bucket, t.name, minio.RemoveObjectOptions{})
	if errd != nil && minio.ToErrorResponse(errd).Code == "NoSuchKey" {
		errd = nil
	}
	// Write an empty marker object to indicate that the thumbnail generation failed.
	_, errp := t.client.PutObject(t.ctx, t.bucket, t.name,
		bytes.NewReader(nil), 0, minio.PutObjectOptions{
			ContentType: echo.MIMEOctetStream,
		})
	if errc != nil {
		return errc
	}
	if errd != nil {
		return errd
	}
	return errp
}

func (ts *thumbsS3) createThumbFile(name, contentType string, meta map[string]string) (vfs.ThumbFiler, error) {
	pr, pw := io.Pipe()

	errCh := make(chan error, 1)
	go func() {
		_, err := ts.client.PutObject(ts.ctx, ts.bucket, name, pr, -1, minio.PutObjectOptions{
			ContentType:  contentType,
			UserMetadata: meta,
		})
		errCh <- err
	}()

	return &s3Thumb{
		pw:     pw,
		errCh:  errCh,
		client: ts.client,
		bucket: ts.bucket,
		name:   name,
		ctx:    ts.ctx,
	}, nil
}

func (ts *thumbsS3) CreateThumb(img *vfs.FileDoc, format string) (vfs.ThumbFiler, error) {
	name := ts.makeName(img.ID(), format)
	meta := map[string]string{
		"file-md5": hex.EncodeToString(img.MD5Sum),
	}
	return ts.createThumbFile(name, "image/jpeg", meta)
}

func (ts *thumbsS3) ThumbExists(img *vfs.FileDoc, format string) (bool, error) {
	name := ts.makeName(img.ID(), format)
	info, err := ts.client.StatObject(ts.ctx, ts.bucket, name, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}
	if md5str, ok := info.UserMetadata["File-Md5"]; ok && md5str != "" {
		md5sum, err := hex.DecodeString(md5str)
		if err == nil && !bytes.Equal(md5sum, img.MD5Sum) {
			return false, nil
		}
	}
	return true, nil
}

func (ts *thumbsS3) RemoveThumbs(img *vfs.FileDoc, formats []string) error {
	objNames := make([]string, len(formats))
	for i, format := range formats {
		objNames[i] = ts.makeName(img.ID(), format)
	}
	return s3util.DeleteObjects(ts.ctx, ts.client, ts.bucket, objNames)
}

func (ts *thumbsS3) ServeThumbContent(w http.ResponseWriter, req *http.Request, img *vfs.FileDoc, format string) error {
	name := ts.makeName(img.ID(), format)
	obj, err := ts.client.GetObject(ts.ctx, ts.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return s3util.WrapNotFound(err)
	}
	defer obj.Close()

	info, err := obj.Stat()
	if err != nil {
		return s3util.WrapNotFound(err)
	}

	if info.ContentType == echo.MIMEOctetStream {
		// We have some old images where the thumbnail has not been correctly
		// saved. We should delete the thumbnail to allow another try.
		if info.Size > 0 {
			_ = ts.RemoveThumbs(img, vfs.ThumbnailFormatNames)
			return os.ErrNotExist
		}
		// Image magick has failed to generate a thumbnail, and retrying would
		// be useless.
		return os.ErrInvalid
	}

	w.Header().Set("Etag", fmt.Sprintf(`"%s"`, info.ETag))
	w.Header().Set("Content-Type", info.ContentType)
	http.ServeContent(w, req, name, unixEpochZero, obj)
	return nil
}

func (ts *thumbsS3) CreateNoteThumb(id, mime, format string) (vfs.ThumbFiler, error) {
	name := ts.makeName(id, format)
	return ts.createThumbFile(name, mime, nil)
}

func (ts *thumbsS3) OpenNoteThumb(id, format string) (io.ReadCloser, error) {
	name := ts.makeName(id, format)
	obj, err := ts.client.GetObject(ts.ctx, ts.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return nil, s3util.WrapNotFound(err)
	}
	// Stat to verify the object actually exists (GetObject doesn't fail on missing keys).
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return obj, nil
}

func (ts *thumbsS3) RemoveNoteThumb(id string, formats []string) error {
	objNames := make([]string, len(formats))
	for i, format := range formats {
		objNames[i] = ts.makeName(id, format)
	}
	err := s3util.DeleteObjects(ts.ctx, ts.client, ts.bucket, objNames)
	if err != nil {
		logger.WithNamespace("vfss3").Infof("Cannot remove note thumbs: %s", err)
	}
	return err
}

func (ts *thumbsS3) ServeNoteThumbContent(w http.ResponseWriter, req *http.Request, id string) error {
	name := ts.makeName(id, consts.NoteImageThumbFormat)
	obj, err := ts.client.GetObject(ts.ctx, ts.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return s3util.WrapNotFound(err)
	}

	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		// Try the original format as fallback.
		name = ts.makeName(id, consts.NoteImageOriginalFormat)
		obj, err = ts.client.GetObject(ts.ctx, ts.bucket, name, minio.GetObjectOptions{})
		if err != nil {
			return s3util.WrapNotFound(err)
		}
		info, err = obj.Stat()
		if err != nil {
			obj.Close()
			return s3util.WrapNotFound(err)
		}
	}
	defer obj.Close()

	w.Header().Set("Etag", fmt.Sprintf(`"%s"`, info.ETag))
	w.Header().Set("Content-Type", info.ContentType)
	http.ServeContent(w, req, name, unixEpochZero, obj)
	return nil
}

func (ts *thumbsS3) makeName(imgID string, format string) string {
	return ts.keyPrefix + fmt.Sprintf("thumbs/%s-%s", makeThumbObjectName(imgID), format)
}

// makeThumbObjectName builds a virtual subfolder structure for thumbnails.
// It splits the 32-char ID into three parts to avoid a flat hierarchy.
// This is the same logic as vfsswift.MakeObjectName (without internalID).
func makeThumbObjectName(docID string) string {
	if len(docID) != 32 {
		return docID
	}
	return docID[:22] + "/" + docID[22:27] + "/" + docID[27:]
}
