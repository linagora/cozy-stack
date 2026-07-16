package config

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationTargetInitsS3WhenGlobalIsSwift(t *testing.T) {
	// A minimal fake S3 endpoint: InitS3Connection only needs a successful
	// ListBuckets call (a signed GET on "/") to consider the connection live;
	// bucket-creation failures are only logged, so any other response is fine.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Owner><ID>test</ID><DisplayName>test</DisplayName></Owner>
  <Buckets></Buckets>
</ListAllMyBucketsResult>`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	endpoint := srv.Listener.Addr().String()

	swiftURL, _ := url.Parse("swift://openstack/")
	s3URL, _ := url.Parse(fmt.Sprintf("s3://%s/?access_key=key&secret_key=secret&bucket_prefix=cozy&use_ssl=false", endpoint))
	config = &Config{Fs: Fs{URL: swiftURL, MigrationTarget: s3URL}}

	require.True(t, HasS3Target())
	// Init the S3 globals from the target even though the global scheme is swift.
	require.NoError(t, InitS3Connection(Fs{URL: MigrationTargetURL()}))
	assert.NotNil(t, GetS3Client())
	assert.Equal(t, "cozy", GetS3BucketPrefix())
}
