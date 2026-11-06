package rabbitmq

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"crypto/tls"
	neturl "net/url"

	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitMQConnection handles RabbitMQ connection management with automatic reconnection.
type RabbitMQConnection struct {
	url       string
	TLSConfig *tls.Config
	conn      *amqp.Connection
	connClose chan *amqp.Error
	mu        sync.RWMutex // Protects conn and connClose fields
}

func NewRabbitMQConnection(url string) *RabbitMQConnection {
	return &RabbitMQConnection{
		url: url,
	}
}

func (cm *RabbitMQConnection) Connect(ctx context.Context, maxRetries int) (*amqp.Connection, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.conn != nil && cm.isConnectionAlive() {
		return cm.conn, nil // Already connected and alive
	}

	// Close existing connection if any
	if cm.conn != nil {
		cm.conn.Close()
		cm.conn = nil
		cm.connClose = nil
	}

	// Create new connection
	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Apply backoff delay (except for first attempt)
		if attempt > 0 {
			backoff := nextBackoff(attempt - 1)
			if err := sleepContext(ctx, backoff); err != nil {
				return nil, err
			}
		}

		// Try to create connection
		var conn *amqp.Connection
		var err error
		if isAMQPS(cm.url) {
			if cm.TLSConfig == nil {
				cm.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}
			}
			conn, err = amqp.DialTLS(cm.url, cm.TLSConfig)
		} else {
			conn, err = amqp.Dial(cm.url)
		}
		if err != nil {
			continue
		}

		// Connection created successfully
		cm.conn = conn
		cm.connClose = make(chan *amqp.Error, 1)
		conn.NotifyClose(cm.connClose)
		return cm.conn, nil
	}

	return nil, fmt.Errorf("failed to connect after %d attempts with urls: %s", maxRetries, connectTarget(cm.url))
}

func connectTarget(u string) string {
	parsed, err := neturl.Parse(u)
	if err != nil || parsed.Host == "" {
		return "(invalid url)"
	}
	return parsed.Host
}

func (cm *RabbitMQConnection) MonitorConnection() <-chan *amqp.Error {
	return cm.connClose
}

func (cm *RabbitMQConnection) isConnectionAlive() bool {
	if cm.conn == nil {
		return false
	}

	// Check if connection is closed by trying to create a temporary channel
	// This is a lightweight way to test connection health
	ch, err := cm.conn.Channel()
	if err != nil {
		return false // Connection is dead
	}

	ch.Close()
	return true
}

func (cm *RabbitMQConnection) Close() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.conn != nil {
		if err := cm.conn.Close(); err != nil {
			return fmt.Errorf("failed to close connection: %w", err)
		}
		cm.conn = nil
		cm.connClose = nil
	}
	return nil
}

func nextBackoff(attempt int) time.Duration {
	base := time.Duration(1<<uint(attempt)) * time.Second
	maxDuration := 30 * time.Second
	if base > maxDuration {
		base = maxDuration
	}
	return withJitter(base)
}

// prevent "thundering herd" effect
func withJitter(d time.Duration) time.Duration {
	if d == 0 {
		return 0
	}
	jitter := time.Duration(rand.Int63n(int64(d / 4)))
	return d + jitter
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isAMQPS(u string) bool {
	parsed, err := neturl.Parse(u)
	if err != nil {
		return false
	}
	return parsed.Scheme == "amqps"
}
