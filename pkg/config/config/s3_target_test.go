package config

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationTargetInitsS3WhenGlobalIsSwift(t *testing.T) {
	previousConfig, previousStorages := config, s3Storages
	t.Cleanup(func() { config, s3Storages = previousConfig, previousStorages })
	s3Storages = nil
	assert.False(t, HasS3Client())

	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, http.MethodHead, r.Method)
		assert.Equal(t, "/migration-storage/", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	v := createTestViper()
	v.Set("fs.url", "swift://openstack/")
	v.Set("fs.migration_target", fmt.Sprintf("s3://%s?access_key=key&secret_key=secret&region=rbx", srv.Listener.Addr()))
	v.Set("fs.s3.auto_create_buckets", false)
	v.Set("fs.s3.buckets.default.name", "migration-storage")
	require.NoError(t, UseViper(v))
	config.Fs.Transport = srv.Client().Transport

	require.True(t, HasS3Target())
	// Use the configured buckets and transport with the alternate endpoint.
	target := config.Fs
	target.URL = MigrationTargetURL()
	require.NoError(t, InitS3Connection(target))
	assert.True(t, HasS3Client())
	assert.Equal(t, SchemeSwift, FsURL().Scheme)
	assert.Equal(t, 1, requests)
	storage := GetS3Storage(S3StorageFiles)
	assert.NotNil(t, storage.Client)
	assert.Equal(t, "migration-storage", storage.Bucket)
	assert.Equal(t, "files/", storage.Prefix)
}
