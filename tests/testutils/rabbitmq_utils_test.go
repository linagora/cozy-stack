package testutils

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsPortBindingError(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    bool
	}{
		{name: "allocated", message: "Bind for 0.0.0.0:5672 failed: port is already allocated", want: true},
		{name: "docker desktop", message: "failed to bind host port 0.0.0.0:5672/tcp: address already in use", want: true},
		{name: "unrelated", message: "failed to start container: permission denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isPortBindingError(tc.message))
		})
	}
}

func TestRabbitMQReadiness(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped with --short")
	}

	mq := StartRabbitMQ(t, false, false)
	checkConnection := func(t *testing.T) {
		conn, ch := CreateRabbitConnection(t, mq)
		defer conn.Close()
		require.NoError(t, ch.Close())
	}

	t.Run("Start", checkConnection)
	mq.Stop(context.Background(), 30*time.Second)
	mq.Restart(context.Background(), 30*time.Second)
	t.Run("Restart", checkConnection)
}
