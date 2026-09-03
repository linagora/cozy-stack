package vfss3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/s3util"
	"github.com/minio/minio-go/v7"
)

// NewAvatarFs creates a new avatar filesystem backed by S3.
func NewAvatarFs(client *minio.Client, bucket, keyPrefix string) vfs.Avatarer {
	return &avatarS3{
		client:    client,
		bucket:    bucket,
		keyPrefix: keyPrefix,
		ctx:       context.Background(),
	}
}

type avatarS3 struct {
	client    *minio.Client
	bucket    string
	keyPrefix string
	ctx       context.Context
}

func (a *avatarS3) avatarKey() string {
	return a.keyPrefix + "avatar"
}

func (a *avatarS3) CreateAvatar(contentType string) (io.WriteCloser, error) {
	key := a.avatarKey()
	pr, pw := io.Pipe()

	meta := map[string]string{
		"created-at": time.Now().UTC().Format(time.RFC3339),
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := a.client.PutObject(a.ctx, a.bucket, key, pr, -1, minio.PutObjectOptions{
			ContentType:  contentType,
			UserMetadata: meta,
		})
		errCh <- err
	}()

	return &avatarWriter{pw: pw, errCh: errCh}, nil
}

type avatarWriter struct {
	pw    *io.PipeWriter
	errCh chan error
}

func (w *avatarWriter) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

func (w *avatarWriter) Close() error {
	if err := w.pw.Close(); err != nil {
		return err
	}
	return <-w.errCh
}

func (a *avatarS3) DeleteAvatar() error {
	err := a.client.RemoveObject(a.ctx, a.bucket, a.avatarKey(), minio.RemoveObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil
		}
		return err
	}
	return nil
}

func (a *avatarS3) ServeAvatarContent(w http.ResponseWriter, req *http.Request) error {
	obj, err := a.client.GetObject(a.ctx, a.bucket, a.avatarKey(), minio.GetObjectOptions{})
	if err != nil {
		return s3util.WrapNotFound(err)
	}
	defer obj.Close()

	info, err := obj.Stat()
	if err != nil {
		return s3util.WrapNotFound(err)
	}

	t := time.Time{}
	if createdAt, ok := info.UserMetadata["Created-At"]; ok && createdAt != "" {
		if createdAtTime, err := time.Parse(time.RFC3339, createdAt); err == nil {
			t = createdAtTime
		}
	}

	w.Header().Set("Etag", fmt.Sprintf(`"%s"`, info.ETag))
	w.Header().Set("Content-Type", info.ContentType)
	http.ServeContent(w, req, "avatar", t, obj)
	return nil
}
