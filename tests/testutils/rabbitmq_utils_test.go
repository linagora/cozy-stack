package testutils

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
