package rabbitmq

import (
	"context"
	"fmt"
	"testing"
	"time"

	"crypto/tls"

	"github.com/stretchr/testify/assert"
)

func TestNewRabbitMQConnection(t *testing.T) {
	url := "amqp://test:test@localhost:5672/"
	cm := NewRabbitMQConnection(url)
	assert.NotNil(t, cm)
	assert.Equal(t, url, cm.url)
	assert.Nil(t, cm.conn)
	assert.Nil(t, cm.connClose)
}

func TestRabbitMQConnection_Connect_InvalidURL(t *testing.T) {
	cm := NewRabbitMQConnection("invalid-url")
	ctx := context.Background()

	conn, err := cm.Connect(ctx, 3)
	assert.Error(t, err)
	assert.Nil(t, conn)
	assert.Contains(t, err.Error(), "failed to connect after 3 attempts")
}

func TestRabbitMQConnection_Connect_ErrorShowsHostOnly(t *testing.T) {
	cm := NewRabbitMQConnection("amqp://cozy:abc123@127.0.0.1:1/vhost")

	_, err := cm.Connect(context.Background(), 1)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "127.0.0.1:1")
	assert.NotContains(t, err.Error(), "cozy")
	assert.NotContains(t, err.Error(), "abc123")

	cm = NewRabbitMQConnection("cozy:abc123@127.0.0.1:1")
	_, err = cm.Connect(context.Background(), 1)
	assert.Error(t, err)
	assert.NotContains(t, err.Error(), "abc123")
}

func TestRabbitMQConnection_Connect_ContextCancelled(t *testing.T) {
	cm := NewRabbitMQConnection("amqp://test:test@localhost:5672/")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	conn, err := cm.Connect(ctx, 3)
	assert.Error(t, err)
	assert.Nil(t, conn)
	assert.Equal(t, context.Canceled, err)
}

func TestRabbitMQConnection_Connect_TLS_WithAMQPSInvalid(t *testing.T) {
	cm := NewRabbitMQConnection("amqps://test:test@localhost:5671/")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	conn, err := cm.Connect(ctx, 3)
	assert.Error(t, err)
	assert.Nil(t, conn)
}

func TestRabbitMQConnection_Connect_TLS_WithConfigInvalid(t *testing.T) {
	cm := NewRabbitMQConnection("amqps://localhost:5671/")
	cm.TLSConfig = &tls.Config{InsecureSkipVerify: false, ServerName: "does-not-exist.invalid"}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	conn, err := cm.Connect(ctx, 3)
	assert.Error(t, err)
	assert.Nil(t, conn)
}

func Test_isAMQPS(t *testing.T) {
	assert.True(t, isAMQPS("amqps://example"))
	assert.False(t, isAMQPS("amqp://example"))
	assert.False(t, isAMQPS("://bad"))
}

func TestRabbitMQConnection_Close_NotConnected(t *testing.T) {
	cm := NewRabbitMQConnection("amqp://test:test@localhost:5672/")
	err := cm.Close()
	assert.NoError(t, err)
}

func TestRabbitMQConnection_Connect_Timeout(t *testing.T) {
	cm := NewRabbitMQConnection("amqp://test:test@localhost:5672/")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	conn, err := cm.Connect(ctx, 5)
	assert.Error(t, err)
	assert.Nil(t, conn)
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

func TestNextBackoff(t *testing.T) {
	testCases := []struct {
		attempt     int
		expectedMin time.Duration
		expectedMax time.Duration
	}{
		{0, 1 * time.Second, 1*time.Second + 250*time.Millisecond},
		{1, 2 * time.Second, 2*time.Second + 500*time.Millisecond},
		{2, 4 * time.Second, 4*time.Second + 1*time.Second},
		{3, 8 * time.Second, 8*time.Second + 2*time.Second},
		{4, 16 * time.Second, 16*time.Second + 4*time.Second},
		{5, 30 * time.Second, 30*time.Second + 7*time.Second + 500*time.Millisecond},  // Max cap
		{10, 30 * time.Second, 30*time.Second + 7*time.Second + 500*time.Millisecond}, // Max cap
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("attempt_%d", tc.attempt), func(t *testing.T) {
			result := nextBackoff(tc.attempt)
			assert.GreaterOrEqual(t, result, tc.expectedMin)
			assert.LessOrEqual(t, result, tc.expectedMax)
		})
	}
}

func TestWithJitter(t *testing.T) {
	base := 4 * time.Second

	// Test multiple times to ensure jitter is working
	for i := 0; i < 10; i++ {
		result := withJitter(base)

		// Jitter should add 0 to 1 second (25% of 4 seconds)
		assert.GreaterOrEqual(t, result, base)
		assert.LessOrEqual(t, result, base+time.Second)
	}
}

func TestWithJitter_ZeroDuration(t *testing.T) {
	result := withJitter(0)
	assert.Equal(t, time.Duration(0), result)
}

func TestRabbitMQConnection_Connect_TLS_SkipVerify(t *testing.T) {
	cm := NewRabbitMQConnection("amqps://localhost:5671/")
	cm.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	conn, err := cm.Connect(ctx, 3)
	assert.Error(t, err)
	assert.Nil(t, conn)
}
