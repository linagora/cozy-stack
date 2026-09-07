package testutils

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ClamAVFixture holds the ClamAV test container and connection information.
type ClamAVFixture struct {
	Container tc.Container
	Address   string // e.g. "127.0.0.1:3310" for clamd TCP connection
	Host      string
	Port      string
	t         *testing.T
}

// StartClamAV starts a ClamAV container with clamd daemon exposed on a random port.
// The container uses the official clamav/clamav image.
// Note: The first startup may take 1-2 minutes as ClamAV downloads virus definitions.
func StartClamAV(t *testing.T) *ClamAVFixture {
	t.Helper()

	req := tc.ContainerRequest{
		Image:         "clamav/clamav:stable",
		ImagePlatform: "linux/amd64", // Force amd64 platform for ARM Macs
		ExposedPorts:  []string{"3310/tcp"},
		Env: map[string]string{
			// Skip freshclam initial update for faster startup in tests
			"CLAMAV_NO_FRESHCLAMD": "true",
		},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("3310/tcp"),
			wait.ForLog("socket found, clamd started").WithStartupTimeout(120*time.Second),
		),
	}

	container, err := tc.GenericContainer(context.Background(), tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "failed to start ClamAV container")

	host, err := container.Host(context.Background())
	require.NoError(t, err, "failed to get host for ClamAV")
	clamdPort, err := container.MappedPort(context.Background(), "3310/tcp")
	require.NoError(t, err, "failed to get port for ClamAV")
	clamdHostPort := clamdPort.Port()

	fixture := &ClamAVFixture{
		Container: container,
		Host:      host,
		Port:      clamdHostPort,
		Address:   fmt.Sprintf("%s:%s", host, clamdHostPort),
		t:         t,
	}

	t.Logf("ClamAV clamd: %s", fixture.Address)

	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	// Wait for clamd to respond to PING
	err = fixture.waitForPing(30 * time.Second)
	require.NoError(t, err, "ClamAV did not respond to PING")

	return fixture
}

// waitForPing waits for clamd to respond to a PING command.
func (f *ClamAVFixture) waitForPing(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.ping() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("clamd did not respond to PING within %s", timeout)
}

// ping sends a PING command to clamd and returns true if it responds with PONG.
func (f *ClamAVFixture) ping() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", f.Address)
	if err != nil {
		return false
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	_, err = conn.Write([]byte("PING\n"))
	if err != nil {
		return false
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return false
	}

	return string(buf[:n]) == "PONG\n"
}

// Restart restarts the ClamAV container.
func (f *ClamAVFixture) Restart(ctx context.Context, timeout time.Duration) {
	f.t.Helper()

	err := f.Container.Start(ctx)
	require.NoError(f.t, err, "failed to start ClamAV container")

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := f.Container.State(ctx)
		require.NoError(f.t, err, "error checking container state")
		if state.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	waitStrategy := wait.ForListeningPort("3310/tcp")
	err = waitStrategy.WaitUntilReady(ctx, f.Container)
	require.NoError(f.t, err, "container did not become ready")

	host, err := f.Container.Host(ctx)
	require.NoError(f.t, err, "can't get host of container")
	port, err := f.Container.MappedPort(ctx, "3310/tcp")
	require.NoError(f.t, err, "can't get clamd port of container")

	f.Host = host
	f.Port = port.Port()
	f.Address = fmt.Sprintf("%s:%s", host, port.Port())

	err = f.waitForPing(timeout)
	require.NoError(f.t, err, "ClamAV did not respond to PING after restart")

	f.t.Logf("ClamAV clamd: %s", f.Address)
}

// Stop stops the ClamAV container.
func (f *ClamAVFixture) Stop(ctx context.Context, timeout time.Duration) {
	err := f.Container.Stop(ctx, &timeout)
	require.NoError(f.t, err)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := f.Container.State(ctx)
		require.NoError(f.t, err, fmt.Errorf("error checking container state: %w", err))
		if !state.Running {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Errorf("container did not stop within %s", timeout)
}

// EICARTestSignature returns the EICAR test file content.
// This is a standard test file that all antivirus software should detect.
// It is NOT a real virus, just a test signature.
// See: https://www.eicar.org/download-anti-malware-testfile/
func EICARTestSignature() []byte {
	return []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)
}
