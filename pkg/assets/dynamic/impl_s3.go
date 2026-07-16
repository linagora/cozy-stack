package dynamic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/assets/model"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/minio/minio-go/v7"
)

// S3FS is the S3 implementation of [AssetsFS].
//
// It saves and fetches assets into/from any S3-compatible object store.
type S3FS struct {
	client *minio.Client
	bucket string
	ctx    context.Context
}

// NewS3FS instantiates a new S3FS.
func NewS3FS() (*S3FS, error) {
	initCacheOnce.Do(func() {
		cache = expirable.NewLRU[string, cacheEntry](1024, nil, 1*time.Hour)
	})

	ctx := context.Background()
	client := config.GetS3Client()
	bucket := config.GetS3BucketPrefix() + "-assets"

	err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: config.GetS3Region()})
	if err != nil {
		code := minio.ToErrorResponse(err).Code
		if code != "BucketAlreadyOwnedByYou" && code != "BucketAlreadyExists" {
			return nil, fmt.Errorf("Cannot create bucket for dynamic assets: %s", err)
		}
	}

	return &S3FS{client: client, bucket: bucket, ctx: ctx}, nil
}

func (s *S3FS) Add(_ string, _ string, asset *model.Asset) error {
	objectName := path.Join(asset.Context, asset.Name)
	data := asset.GetData()
	_, err := s.client.PutObject(s.ctx, s.bucket, objectName,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{})
	return err
}

func (s *S3FS) Get(ctx string, name string) ([]byte, error) {
	objectName := path.Join(ctx, name)
	if entry, ok := cache.Get(objectName); ok {
		if !entry.found {
			return nil, os.ErrNotExist
		}
		return entry.content, nil
	}

	obj, err := s.client.GetObject(s.ctx, s.bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()

	content, err := io.ReadAll(obj)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			cache.Add(objectName, cacheEntry{found: false})
			return nil, os.ErrNotExist
		}
		return nil, err
	}

	cache.Add(objectName, cacheEntry{found: true, content: content})
	return content, nil
}

func (s *S3FS) Remove(context, name string) error {
	objectName := path.Join(context, name)
	return s.client.RemoveObject(s.ctx, s.bucket, objectName, minio.RemoveObjectOptions{})
}

func (s *S3FS) List() (map[string][]*model.Asset, error) {
	objs := map[string][]*model.Asset{}

	for obj := range s.client.ListObjects(s.ctx, s.bucket, minio.ListObjectsOptions{
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}

		splitted := strings.SplitN(obj.Key, "/", 2)
		if len(splitted) < 2 {
			continue
		}
		ctx := splitted[0]
		assetName := model.NormalizeAssetName(splitted[1])

		a, err := GetAsset(ctx, assetName)
		if err != nil {
			return nil, err
		}

		objs[ctx] = append(objs[ctx], a)
	}

	return objs, nil
}

func (s *S3FS) CheckStatus(ctx context.Context) (time.Duration, error) {
	before := time.Now()
	_, err := s.client.ListBuckets(ctx)
	if err != nil {
		return 0, err
	}
	return time.Since(before), nil
}
