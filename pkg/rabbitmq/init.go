package rabbitmq

import (
	"context"
	"sync"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/logger"
)

var log = logger.WithNamespace("rabbitmq")

// Service handles all the interactions with RabbitMQ
//
// Several implementations exist:
// - [RabbitMQService] interacts with the RabbitMQ nodes
// - [NoopService] when no config is setup
type Service interface {
	StartManagers() ([]*RabbitMQManager, error)
	Publish(ctx context.Context, req PublishRequest) error
}

var (
	defaultMu      sync.RWMutex
	defaultService Service = new(NoopService)
)

func Init(cfg config.RabbitMQ) (Service, error) {
	if !cfg.Enabled || cfg.Nodes == nil {
		svc := new(NoopService)
		SetDefault(svc)
		return svc, nil
	}

	svc, err := NewService(cfg)
	if err != nil {
		return nil, err
	}
	SetDefault(svc)
	return svc, nil
}

// Default returns the service built by the last Init. The job workers use it,
// since they have no access to the stack services.
func Default() Service {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultService
}

// SetDefault replaces the default service, and returns a func restoring the
// previous one.
func SetDefault(s Service) func() {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	prev := defaultService
	defaultService = s
	return func() { SetDefault(prev) }
}
