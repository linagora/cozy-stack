// Package s3util provides shared helpers for interacting with S3-compatible
// object stores via the minio-go client.
package s3util

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/minio/minio-go/v7"
)

// IsNotFound returns true when the error is an S3 "not found" response
// (NoSuchKey or NoSuchBucket).
func IsNotFound(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NoSuchBucket"
}

// WrapNotFound converts S3 not-found errors to os.ErrNotExist and sanitizes
// other S3 errors to avoid leaking internal bucket/key details.
func WrapNotFound(err error) error {
	if IsNotFound(err) {
		return os.ErrNotExist
	}
	code := minio.ToErrorResponse(err).Code
	if code != "" {
		return fmt.Errorf("s3 storage error: %s", code)
	}
	return err
}

// EnsureBucket creates the bucket if it does not already exist.
func EnsureBucket(ctx context.Context, client *minio.Client, bucket, region string) error {
	err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{
		Region: region,
	})
	if err != nil {
		code := minio.ToErrorResponse(err).Code
		if code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists" {
			return nil
		}
		return err
	}
	return nil
}

// DeleteObjects deletes a list of named objects from a bucket.
func DeleteObjects(ctx context.Context, client *minio.Client, bucket string, objNames []string) error {
	if len(objNames) == 0 {
		return nil
	}
	objectsCh := make(chan minio.ObjectInfo, len(objNames))
	for _, name := range objNames {
		objectsCh <- minio.ObjectInfo{Key: name}
	}
	close(objectsCh)
	var errm error
	for e := range client.RemoveObjects(ctx, bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		errm = errors.Join(errm, e.Err)
	}
	return errm
}

// DeletePrefixObjects deletes all objects in a bucket under a given prefix.
func DeletePrefixObjects(ctx context.Context, client *minio.Client, bucket, prefix string) error {
	objectsCh := make(chan minio.ObjectInfo)
	var listErr error
	go func() {
		defer close(objectsCh)
		for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
		}) {
			if obj.Err != nil {
				listErr = obj.Err
				return
			}
			objectsCh <- obj
		}
	}()
	var errm error
	for e := range client.RemoveObjects(ctx, bucket, objectsCh, minio.RemoveObjectsOptions{}) {
		errm = errors.Join(errm, e.Err)
	}
	if listErr != nil {
		return listErr
	}
	return errm
}
