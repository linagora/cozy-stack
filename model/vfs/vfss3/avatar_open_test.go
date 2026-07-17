package vfss3_test

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/cozy/cozy-stack/model/vfs/vfss3"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenAvatarRoundTrip verifies that OpenAvatar returns the content and
// content-type previously stored via CreateAvatar, and that it reports
// os.ErrNotExist when no avatar has been stored yet.
func TestOpenAvatarRoundTrip(t *testing.T) {
	config.UseTestFile(t)

	mf := testutils.StartMinio(t)

	require.NoError(t, config.InitS3Connection(config.Fs{URL: mf.FsURL("test")}))

	bucket := "io-cozy-vfss3-openavatar-test"
	keyPrefix := "io.cozy.vfss3.openavatar.test/"

	client := mf.Client(t)
	require.NoError(t, client.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}))

	av := vfss3.NewAvatarFs(client, bucket, keyPrefix)

	// No avatar stored yet: OpenAvatar must report os.ErrNotExist.
	_, _, err := av.OpenAvatar()
	assert.ErrorIs(t, err, os.ErrNotExist)

	// Store an avatar, then read it back.
	w, err := av.CreateAvatar("image/png")
	require.NoError(t, err)
	payload := []byte("fake png bytes")
	_, err = w.Write(payload)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r, ctype, err := av.OpenAvatar()
	require.NoError(t, err)
	defer r.Close()
	assert.Equal(t, "image/png", ctype)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}
