package config

import (
	"context"
	"fmt"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var s3Client *minio.Client
var s3BucketPrefix string
var s3Region string

// InitDefaultS3Connection initializes the default S3 handler.
func InitDefaultS3Connection() error {
	return InitS3Connection(config.Fs)
}

// InitS3Connection initializes the global S3 client connection. This is
// not a thread-safe method.
func InitS3Connection(fs Fs) error {
	fsURL := fs.URL
	if fsURL.Scheme != SchemeS3 {
		return nil
	}

	q := fsURL.Query()
	endpoint := fsURL.Host
	accessKey := q.Get("access_key")
	secretKey := q.Get("secret_key")
	region := q.Get("region")
	useSSL := q.Get("use_ssl") != "false" // default true

	s3BucketPrefix = q.Get("bucket_prefix")
	if s3BucketPrefix == "" {
		s3BucketPrefix = "cozy"
	}
	// Sanitize bucket prefix: lowercase, only alphanumeric and hyphens
	s3BucketPrefix = strings.ToLower(s3BucketPrefix)
	s3BucketPrefix = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, s3BucketPrefix)
	s3BucketPrefix = strings.Trim(s3BucketPrefix, "-")
	s3Region = region

	var err error
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: region,
	}
	if fs.Transport != nil {
		opts.Transport = fs.Transport
	}

	s3Client, err = minio.New(endpoint, opts)
	if err != nil {
		return fmt.Errorf("s3: could not create client: %w", err)
	}

	// Verify connectivity by listing buckets
	if _, err = s3Client.ListBuckets(context.Background()); err != nil {
		log.Errorf("Could not connect to S3 endpoint %s: %s", endpoint, err)
		return err
	}

	log.Infof("Successfully connected to S3 endpoint %s", endpoint)

	// Pre-create the fixed buckets used by secondary storage (apps, assets,
	// previews, exports). The per-org VFS bucket is created on instance init.
	ctx := context.Background()
	for _, suffix := range []string{"-apps-web", "-apps-konnectors", "-assets", "-previews", "-exports"} {
		bucket := s3BucketPrefix + suffix
		if err := s3Client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region}); err != nil {
			code := minio.ToErrorResponse(err).Code
			if code != "BucketAlreadyOwnedByYou" && code != "BucketAlreadyExists" {
				log.Warnf("Could not create bucket %s: %s", bucket, err)
			}
		}
	}

	return nil
}

// GetS3Client returns the global S3 client.
func GetS3Client() *minio.Client {
	if s3Client == nil {
		panic("Called GetS3Client() before InitS3Connection()")
	}
	return s3Client
}

// GetS3BucketPrefix returns the configured bucket prefix.
func GetS3BucketPrefix() string {
	return s3BucketPrefix
}

// GetS3Region returns the configured S3 region.
func GetS3Region() string {
	return s3Region
}
