package testutils

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"

	c "github.com/docker/docker/api/types/container"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// SwiftFixture holds the state for a running OpenStack Swift container. Unlike
// the in-memory swifttest server, it has the real behavior of the Static
// Large Objects (copy, delete with multipart-manifest=delete, ranges).
type SwiftFixture struct {
	Container tc.Container
	Endpoint  string // host:port
}

// StartSwift starts an all-in-one Swift container for testing.
func StartSwift(t *testing.T) *SwiftFixture {
	t.Helper()

	hostPort := getFreePort(t)
	req := tc.ContainerRequest{
		Image:        "openstackswift/saio:latest",
		ExposedPorts: []string{"8080/tcp"},
		HostConfigModifier: func(hc *c.HostConfig) {
			// Swift refuses to write when its disk has less than 1% free.
			hc.Tmpfs = map[string]string{"/srv/node": "rw,size=1g"}
			hc.PortBindings = nat.PortMap{
				"8080/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: hostPort}},
			}
		},
		WaitingFor: wait.ForHTTP("/info").
			WithPort("8080/tcp").
			WithStartupTimeout(120 * time.Second),
	}
	container, err := tc.GenericContainer(context.Background(), tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "failed to start Swift")

	host, err := container.Host(context.Background())
	require.NoError(t, err)
	endpoint := fmt.Sprintf("%s:%s", host, hostPort)
	t.Logf("Swift endpoint: %s", endpoint)

	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})
	// The proxy answers before the account and container servers are ready.
	require.Eventually(t, func() bool { return swiftReady(endpoint) }, 120*time.Second, time.Second,
		"Swift is not ready")
	return &SwiftFixture{Container: container, Endpoint: endpoint}
}

func swiftReady(endpoint string) bool {
	req, _ := http.NewRequest(http.MethodGet, "http://"+endpoint+"/auth/v1.0", nil)
	req.Header.Set("X-Auth-User", "test:tester")
	req.Header.Set("X-Auth-Key", "testing")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	token, storage := res.Header.Get("X-Auth-Token"), res.Header.Get("X-Storage-Url")
	if token == "" || storage == "" {
		return false
	}
	req, _ = http.NewRequest(http.MethodPut, storage+"/readiness", nil)
	req.Header.Set("X-Auth-Token", token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode/100 == 2
}

// FsURL returns a *url.URL suitable for config.InitSwiftConnection.
func (f *SwiftFixture) FsURL() *url.URL {
	q := url.Values{
		"UserName": {"test:tester"},
		"Password": {"testing"},
		"AuthURL":  {"http://" + f.Endpoint + "/auth/v1.0"},
	}
	return &url.URL{Scheme: "swift", Host: f.Endpoint, RawQuery: q.Encode()}
}
