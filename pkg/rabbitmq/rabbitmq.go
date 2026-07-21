package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"crypto/tls"
	"crypto/x509"
	"os"

	"github.com/cozy/cozy-stack/pkg/config/config"
	amqp "github.com/rabbitmq/amqp091-go"
	"golang.org/x/sync/errgroup"
)

// Handler processes messages for a queue. Return nil to ack, or an error to requeue.
type Handler interface {
	Handle(ctx context.Context, d amqp.Delivery) error
}

type QueueSpec struct {
	cfg     *config.RabbitQueue
	Handler Handler // business logic
	dlxName string  // resolved DLX (queue override or exchange default)
	dlqName string  // resolved DLQ (queue override or exchange default)
}

type ExchangeSpec struct {
	cfg    *config.RabbitExchange
	Queues []QueueSpec
}

type RabbitMQManager struct {
	connection *RabbitMQConnection
	exchanges  []ExchangeSpec
	cancel     context.CancelFunc
	done       chan struct{}
	readyCh    chan struct{}
	readyOnce  sync.Once
}

func NewRabbitMQManager(connection *RabbitMQConnection, exchanges []ExchangeSpec) *RabbitMQManager {
	return &RabbitMQManager{
		connection: connection,
		exchanges:  exchanges,
		readyCh:    make(chan struct{}),
	}
}

// NewExchangeSpec creates a new ExchangeSpec with the given configuration
func NewExchangeSpec(cfg *config.RabbitExchange) ExchangeSpec {
	return ExchangeSpec{
		cfg:    cfg,
		Queues: []QueueSpec{},
	}
}

// NewQueueSpec creates a new QueueSpec with the given configuration and handler
func NewQueueSpec(cfg *config.RabbitQueue, handler Handler, dlxName, dlqName string) QueueSpec {
	return QueueSpec{
		cfg:     cfg,
		Handler: handler,
		dlxName: dlxName,
		dlqName: dlqName,
	}
}

// Start runs the manager in background and returns itself
func (m *RabbitMQManager) Start(ctx context.Context) (*RabbitMQManager, error) {
	log.Info("Initializing RabbitMQ manager")
	if m.readyCh == nil {
		m.readyCh = make(chan struct{})
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.run(ctx)
	}()
	m.cancel = cancel
	m.done = done
	log.Info("RabbitMQ manager started successfully")
	return m, nil
}

// WaitReady blocks until the manager has declared exchanges/queues and started consumers.
func (m *RabbitMQManager) WaitReady(ctx context.Context) error {
	log.Info("Waiting for RabbitMQ manager to be ready")
	select {
	case <-m.readyCh:
		log.Info("RabbitMQ manager is ready")
		return nil
	case <-ctx.Done():
		log.Warnf("RabbitMQ manager wait ready cancelled: %v", ctx.Err())
		return ctx.Err()
	}
}

// RabbitMQManager implements utils.Shutdowner for a graceful stop
func (m *RabbitMQManager) Shutdown(ctx context.Context) error {
	log.Info("Shutting down RabbitMQ manager")
	if m.cancel != nil {
		log.Debug("Cancelling RabbitMQ manager context")
		m.cancel()
	}
	if m.done != nil {
		log.Debug("Waiting for RabbitMQ manager to finish")
		select {
		case <-m.done:
			log.Debug("RabbitMQ manager finished gracefully")
		case <-ctx.Done():
			log.Warnf("RabbitMQ manager shutdown cancelled: %v", ctx.Err())
			return ctx.Err()
		}
	}
	log.Debug("Closing RabbitMQ connection")
	err := m.connection.Close()
	if err != nil {
		log.Errorf("Error closing RabbitMQ connection: %v", err)
	} else {
		log.Info("RabbitMQ manager shutdown completed successfully")
	}
	return err
}

// run starts the manager loop with automatic reconnection.
func (m *RabbitMQManager) run(ctx context.Context) error {
	log.Infof("Starting RabbitMQ manager with %d exchanges", len(m.exchanges))

	// Main manager loop with automatic reconnection
	reconnectCount := 0
	for {
		select {
		case <-ctx.Done():
			log.Info("Context canceled, shutting down manager")
			return m.connection.Close()
		default:
		}

		reconnectCount++
		if reconnectCount > 1 {
			log.Infof("Attempting reconnection #%d to RabbitMQ", reconnectCount)
		}

		// Connect to RabbitMQ with retry
		log.Debug("Connecting to RabbitMQ")
		conn, err := m.connection.Connect(ctx, 5)
		if err != nil {
			log.Errorf("Failed to connect to RabbitMQ after retries: %v", err)
			continue
		}
		log.Info("Successfully connected to RabbitMQ")

		// Declare exchanges once per cycle
		log.Debug("Declaring exchanges")
		for _, spec := range m.exchanges {
			log.Debugf("Declaring exchange: %s (type: %s, durable: %v)", spec.cfg.Name, spec.cfg.Kind, spec.cfg.Durable)
			if err := declareRabbitMQExchange(conn, spec); err != nil {
				log.Errorf("Failed to declare exchange %s: %v", spec.cfg.Name, err)
				_ = m.connection.Close()
				continue
			}
			log.Debugf("Successfully declared exchange: %s", spec.cfg.Name)
		}

		g, gctx := errgroup.WithContext(ctx)
		// Start all queue runners across all exchanges
		log.Debug("Starting queue runners")
		var queueRunners []*queueRunner
		for _, spec := range m.exchanges {
			log.Debugf("Processing %d queues for exchange: %s", len(spec.Queues), spec.cfg.Name)
			for _, q := range spec.Queues {
				log.Debugf("Starting queue runner for queue: %s on exchange: %s", q.cfg.Name, spec.cfg.Name)
				r, err := newQueueRunner(conn, spec.cfg.Name, q)
				if err != nil {
					log.Errorf("Failed to start queue runner for %s on %s: %v", q.cfg.Name, spec.cfg.Name, err)
					continue
				}
				queueRunners = append(queueRunners, r)
				log.Debugf("Successfully started queue runner for: %s", q.cfg.Name)
			}
		}
		log.Infof("Started %d queue runners", len(queueRunners))
		for _, r := range queueRunners {
			r := r
			g.Go(func() error { return r.run(gctx) })
		}
		// Signal readiness once topology is set and consumers are started
		m.readyOnce.Do(func() {
			log.Info("RabbitMQ manager topology ready, signaling readiness")
			close(m.readyCh)
		})
		// Monitor connection in the same errgroup; exit on context cancel too
		g.Go(func() error {
			select {
			case <-gctx.Done():
				log.Debug("Connection monitor context cancelled")
				return gctx.Err()
			case err := <-m.connection.MonitorConnection():
				if err != nil {
					log.Warnf("RabbitMQ connection lost: %v", err)
					return err
				}
				log.Debug("RabbitMQ connection monitor received close signal")
				return context.Canceled
			}
		})

		log.Debug("Waiting for queue runners and connection monitor")
		if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
			log.Errorf("Manager cycle error: %v", err)
		}

		// Close all queue runners and connection before next reconnect attempt
		log.Debug("Closing queue runners and connection")
		for _, r := range queueRunners {
			r.close()
		}
		_ = m.connection.Close()
		log.Info("RabbitMQ manager cycle completed, will attempt reconnection")
	}
}

func BuildConnection(node config.RabbitMQNode) (*RabbitMQConnection, error) {
	connection := NewRabbitMQConnection(node.URL)

	tlsCfg, err := buildRabbitTLS(node.TLS)
	if err != nil {
		return nil, err
	}
	connection.TLSConfig = tlsCfg

	return connection, nil
}

func BuildExchangeSpecs(exchangesCfg []config.RabbitExchange) []ExchangeSpec {
	var exchanges []ExchangeSpec

	for i := range exchangesCfg {
		configExchange := &exchangesCfg[i]
		var queues []QueueSpec

		if configExchange.Name == "" || configExchange.Kind == "" {
			log.Warnf("Skipping invalid exchange config (missing name/kind)")
			continue
		}

		for j := range configExchange.Queues {
			configQueue := &configExchange.Queues[j]
			if configQueue.Name == "" {
				log.Warnf("Skipping invalid queue config on exchange %s: missing name", configExchange.Name)
				continue
			}
			var handler Handler

			// Map queue names to appropriate handlers
			switch configQueue.Name {
			case QueueUserPasswordUpdated:
				handler = NewPasswordChangeHandler()
			case QueueUserCreated:
				handler = NewUserCreatedHandler()
			case QueueUserPhoneUpdated:
				handler = NewUserPhoneUpdatedHandler()
			case QueueUser2FAUpdated:
				handler = NewUser2FAUpdatedHandler()
			case QueueUserRecoveryEmailUpdated:
				handler = NewUserRecoveryEmailUpdatedHandler()
			case QueueDomainSubscriptionChanged:
				handler = NewDomainSubscriptionChangedHandler()
			case QueueB2BUserDeleted:
				handler = NewUserDeletedHandler()
			case QueueB2BGroupLifecycle:
				handler = NewB2BGroupLifecycleHandler()
			case QueueAppCommands:
				handler = NewAppInstallHandler()
			}

			if handler == nil {
				log.Warnf("Skipping queue without handler on exchange %s: %s", configExchange.Name, configQueue.Name)
				continue
			}

			// Resolve DLX/DLQ with queue-level override first, else exchange default
			resolvedDLX := configQueue.DLXName
			if resolvedDLX == "" {
				resolvedDLX = configExchange.DLXName
			}
			resolvedDLQ := configQueue.DLQName
			if resolvedDLQ == "" {
				resolvedDLQ = configExchange.DLQName
			}

			queues = append(queues, QueueSpec{
				cfg:     configQueue,
				Handler: handler,
				dlxName: resolvedDLX,
				dlqName: resolvedDLQ,
			})
		}

		exchanges = append(exchanges, ExchangeSpec{
			cfg:    configExchange,
			Queues: queues,
		})
	}

	return exchanges
}

func declareRabbitMQExchange(conn *amqp.Connection, spec ExchangeSpec) error {
	if spec.cfg.DeclareExchange {
		ch, err := conn.Channel()
		if err != nil {
			return fmt.Errorf("failed to open channel for exchange %s: %w", spec.cfg.Name, err)
		}
		defer ch.Close()

		if err := ch.ExchangeDeclare(spec.cfg.Name, spec.cfg.Kind, spec.cfg.Durable, false, false, false, nil); err != nil {
			return fmt.Errorf("failed to declare exchange %s: %w", spec.cfg.Name, err)
		}
	}
	return nil
}

func buildRabbitTLS(tlsOpt config.RabbitMQTLS) (*tls.Config, error) {
	if tlsOpt.RootCAFile == "" && !tlsOpt.InsecureSkipValidation && tlsOpt.ServerName == "" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if tlsOpt.ServerName != "" {
		cfg.ServerName = tlsOpt.ServerName
	}
	if tlsOpt.InsecureSkipValidation {
		cfg.InsecureSkipVerify = true
	}
	if tlsOpt.RootCAFile != "" {
		caCertPEM, err := os.ReadFile(tlsOpt.RootCAFile)
		if err != nil {
			return nil, fmt.Errorf("rabbitmq tls: read root_ca: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caCertPEM) {
			return nil, fmt.Errorf("rabbitmq tls: failed to append root_ca")
		}
		cfg.RootCAs = roots
	}
	return cfg, nil
}
