package testutils

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	c "github.com/docker/docker/api/types/container"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// MinioFixture holds the state for a running MinIO container.
type MinioFixture struct {
	Container tc.Container
	Endpoint  string // host:port
	AccessKey string
	SecretKey string
	t         *testing.T
}

// StartMinio starts a MinIO container for testing.
func StartMinio(t *testing.T) *MinioFixture {
	t.Helper()

	accessKey := "minioadmin"
	secretKey := "minioadmin"
	hostPort := getFreePort(t)

	req := tc.ContainerRequest{
		Image:        "minio/minio:RELEASE.2025-02-28T09-55-16Z",
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"MINIO_ROOT_USER":     accessKey,
			"MINIO_ROOT_PASSWORD": secretKey,
		},
		Cmd: []string{"server", "/data"},
		HostConfigModifier: func(hc *c.HostConfig) {
			hc.PortBindings = nat.PortMap{
				"9000/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: hostPort}},
			}
		},
		WaitingFor: wait.ForHTTP("/minio/health/live").
			WithPort("9000/tcp").
			WithStartupTimeout(60 * time.Second),
	}

	container, err := tc.GenericContainer(context.Background(), tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "failed to start MinIO")

	host, err := container.Host(context.Background())
	require.NoError(t, err)

	endpoint := fmt.Sprintf("%s:%s", host, hostPort)
	t.Logf("MinIO endpoint: %s", endpoint)

	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	return &MinioFixture{
		Container: container,
		Endpoint:  endpoint,
		AccessKey: accessKey,
		SecretKey: secretKey,
		t:         t,
	}
}

// Client returns a minio.Client connected to this fixture.
func (f *MinioFixture) Client(t *testing.T) *minio.Client {
	t.Helper()
	client, err := minio.New(f.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(f.AccessKey, f.SecretKey, ""),
		Secure: false,
	})
	require.NoError(t, err)
	return client
}

// FsURL returns a *url.URL suitable for config.InitS3Connection.
func (f *MinioFixture) FsURL(bucketPrefix string) *url.URL {
	return &url.URL{
		Scheme:   "s3",
		Host:     f.Endpoint,
		RawQuery: fmt.Sprintf("access_key=%s&secret_key=%s&bucket_prefix=%s&use_ssl=false", f.AccessKey, f.SecretKey, bucketPrefix),
	}
}
