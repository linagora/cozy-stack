package s3util_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/cozy/cozy-stack/pkg/s3util"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestS3Util(t *testing.T) {
	if testing.Short() {
		t.Skip("requires minio container: skipped with --short")
	}

	fixture := testutils.StartMinio(t)
	client := fixture.Client(t)
	ctx := context.Background()
	bucket := "s3util-test"

	require.NoError(t, client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))
	t.Cleanup(func() {
		// Best-effort cleanup: remove all objects then the bucket.
		for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = client.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{})
		}
		_ = client.RemoveBucket(ctx, bucket)
	})

	t.Run("IsNotFound", func(t *testing.T) {
		_, err := client.GetObject(ctx, bucket, "does-not-exist", minio.GetObjectOptions{})
		require.NoError(t, err) // GetObject itself doesn't fail; Stat does.

		_, err = client.StatObject(ctx, bucket, "does-not-exist", minio.StatObjectOptions{})
		require.Error(t, err)
		assert.True(t, s3util.IsNotFound(err))

		// A non-not-found error should return false.
		assert.False(t, s3util.IsNotFound(io.ErrUnexpectedEOF))
	})

	t.Run("WrapNotFound", func(t *testing.T) {
		_, err := client.StatObject(ctx, bucket, "does-not-exist", minio.StatObjectOptions{})
		require.Error(t, err)

		wrapped := s3util.WrapNotFound(err)
		assert.ErrorIs(t, wrapped, os.ErrNotExist)

		// A non-S3 error passes through unchanged.
		orig := io.ErrUnexpectedEOF
		assert.Equal(t, orig, s3util.WrapNotFound(orig))
	})

	t.Run("EnsureBucket", func(t *testing.T) {
		newBucket := "s3util-ensure-test"
		t.Cleanup(func() { _ = client.RemoveBucket(ctx, newBucket) })

		// First call creates.
		err := s3util.EnsureBucket(ctx, client, newBucket, "")
		assert.NoError(t, err)

		// Second call is idempotent.
		err = s3util.EnsureBucket(ctx, client, newBucket, "")
		assert.NoError(t, err)
	})

	t.Run("DeleteObjects", func(t *testing.T) {
		// Create a few objects.
		for _, key := range []string{"del-a", "del-b", "del-c"} {
			_, err := client.PutObject(ctx, bucket, key,
				bytes.NewReader([]byte("x")), 1,
				minio.PutObjectOptions{})
			require.NoError(t, err)
		}

		err := s3util.DeleteObjects(ctx, client, bucket, []string{"del-a", "del-b", "del-c"})
		assert.NoError(t, err)

		// Verify they are gone.
		for _, key := range []string{"del-a", "del-b", "del-c"} {
			_, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
			assert.True(t, s3util.IsNotFound(err), "object %s should be deleted", key)
		}
	})

	t.Run("DeleteObjectsEmpty", func(t *testing.T) {
		// Should be a no-op, not an error.
		assert.NoError(t, s3util.DeleteObjects(ctx, client, bucket, nil))
		assert.NoError(t, s3util.DeleteObjects(ctx, client, bucket, []string{}))
	})

	t.Run("DeletePrefixObjects", func(t *testing.T) {
		// Create objects under a prefix and one outside.
		for _, key := range []string{"pfx/one", "pfx/two", "pfx/sub/three", "outside"} {
			_, err := client.PutObject(ctx, bucket, key,
				bytes.NewReader([]byte("x")), 1,
				minio.PutObjectOptions{})
			require.NoError(t, err)
		}

		err := s3util.DeletePrefixObjects(ctx, client, bucket, "pfx/")
		assert.NoError(t, err)

		// Prefixed objects should be gone.
		for _, key := range []string{"pfx/one", "pfx/two", "pfx/sub/three"} {
			_, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
			assert.True(t, s3util.IsNotFound(err), "object %s should be deleted", key)
		}

		// Object outside the prefix should still exist.
		_, err = client.StatObject(ctx, bucket, "outside", minio.StatObjectOptions{})
		assert.NoError(t, err)
	})
}
