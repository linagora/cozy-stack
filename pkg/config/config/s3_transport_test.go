package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A frozen S3 accepts connections and never answers: the transport given by
// the configuration must give up waiting for the response headers.
func TestS3TransportBoundsResponseHeaders(t *testing.T) {
	previous, timeout := s3Storages, s3ResponseHeaderTimeout
	t.Cleanup(func() { s3Storages, s3ResponseHeaderTimeout = previous, timeout })
	s3ResponseHeaderTimeout = 100 * time.Millisecond

	for _, preferIPv4 := range []bool{false, true} {
		t.Run(fmt.Sprint("prefer_ipv4=", preferIPv4), func(t *testing.T) {
			var frozen atomic.Bool
			var requests atomic.Int64
			thaw := make(chan struct{})
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if frozen.Load() {
					requests.Add(1)
					<-thaw
				}
			}))
			defer endpoint.Close()
			defer close(thaw)

			u, err := url.Parse(strings.Replace(endpoint.URL, "http:", "s3:", 1))
			require.NoError(t, err)
			u.RawQuery = url.Values{"access_key": {"KEY"}, "secret_key": {"SECRET"}, "region": {"us-east-1"}, "use_ssl": {"false"}}.Encode()
			disabled := false
			require.NoError(t, InitS3Connection(Fs{
				URL:       u,
				Transport: http.DefaultTransport.(*http.Transport).Clone(),
				S3: FsS3{
					PreferIPv4:        preferIPv4,
					AutoCreateBuckets: &disabled,
					Buckets:           map[string]FsS3Bucket{"default": {Name: "company-storage"}},
				},
			}))

			frozen.Store(true)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			storage := GetS3Storage(S3StorageFiles)
			_, err = storage.Client.StatObject(ctx, storage.Bucket, "object", minio.StatObjectOptions{})
			require.Error(t, err)
			assert.Greater(t, requests.Load(), int64(1), "the request is retried once the headers timed out")
		})
	}
}
