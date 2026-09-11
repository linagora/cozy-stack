package config

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/s3util"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

var s3StorageKinds = []string{"files", "apps_web", "apps_konnectors", "assets", "previews", "exports"}
var s3Storages map[string]S3Storage

// S3Storage identifies a storage type's connection, bucket and object namespace.
type S3Storage struct {
	Client *minio.Client
	Bucket string
	Prefix string
}

// InitDefaultS3Connection initializes the default S3 handler.
func InitDefaultS3Connection() error {
	return InitS3Connection(config.Fs)
}

// InitS3Connection validates the storage layout and initializes each distinct
// bucket. It must be called before serving requests and is not thread-safe.
func InitS3Connection(fs Fs) error {
	if fs.URL == nil || fs.URL.Scheme != SchemeS3 {
		return nil
	}
	for kind, entry := range fs.S3.Buckets {
		if kind != "default" && !slices.Contains(s3StorageKinds, kind) {
			return fmt.Errorf("s3: unknown storage type %q in fs.s3.buckets", kind)
		}
		if s3utils.CheckValidBucketNameStrict(entry.Name) != nil {
			return fmt.Errorf("s3: invalid bucket name for fs.s3.buckets.%s", kind)
		}
	}
	storages := make(map[string]S3Storage, len(s3StorageKinds))
	clients := make(map[string]*minio.Client)
	var destinations []S3Storage
	for _, kind := range s3StorageKinds {
		entry, ok := fs.S3.Buckets[kind]
		if !ok {
			entry, ok = fs.S3.Buckets["default"]
		}
		if !ok {
			return fmt.Errorf("s3: missing bucket for storage type %q; set fs.s3.buckets.default or fs.s3.buckets.%s", kind, kind)
		}
		u := fs.URL
		if entry.URL != "" {
			var err error
			u, err = url.Parse(entry.URL)
			if err != nil {
				return fmt.Errorf("s3: invalid connection URL for bucket %q", entry.Name)
			}
		}
		connection := u.String()
		client := clients[connection]
		if client == nil {
			var err error
			client, err = newS3Client(u, fs.Transport)
			if err != nil {
				return fmt.Errorf("s3: invalid connection URL for bucket %q", entry.Name)
			}
			clients[connection] = client
		}
		storage := S3Storage{Client: client, Bucket: entry.Name, Prefix: strings.ReplaceAll(kind, "_", "-") + "/"}
		storages[kind] = storage
		if !slices.ContainsFunc(destinations, func(other S3Storage) bool {
			return other.Client == client && other.Bucket == entry.Name
		}) {
			destinations = append(destinations, storage)
		}
	}
	for _, storage := range destinations {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var err error
		if fs.S3.AutoCreateBuckets == nil || *fs.S3.AutoCreateBuckets {
			// MinIO uses the client's configured region when this region is empty.
			err = s3util.EnsureBucket(ctx, storage.Client, storage.Bucket, "")
			if err != nil {
				err = fmt.Errorf("s3: could not create bucket %q; check endpoint, credentials and permissions", storage.Bucket)
			}
		} else {
			err = s3util.CheckBucket(ctx, storage.Client, storage.Bucket)
		}
		cancel()
		if err != nil {
			return err
		}
	}
	s3Storages = storages
	log.Infof("Successfully connected to S3 endpoint %s", fs.URL.Host)
	return nil
}

func newS3Client(u *url.URL, transport http.RoundTripper) (*minio.Client, error) {
	if u.Scheme != SchemeS3 {
		return nil, errors.New("s3: expected an s3:// connection URL")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("s3: invalid connection parameters")
	}
	return minio.New(u.Host, &minio.Options{
		Creds:     credentials.NewStaticV4(q.Get("access_key"), q.Get("secret_key"), ""),
		Secure:    q.Get("use_ssl") != "false",
		Region:    q.Get("region"),
		Transport: transport,
	})
}

// GetS3Storage resolves a known storage type without contacting S3.
func GetS3Storage(kind string) S3Storage {
	storage, ok := s3Storages[kind]
	if !ok {
		panic("S3 storage is not initialized for " + kind)
	}
	return storage
}
