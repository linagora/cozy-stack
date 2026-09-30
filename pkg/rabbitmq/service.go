package rabbitmq

import (
	"context"
	"fmt"

	"github.com/cozy/cozy-stack/pkg/config/config"
)

type RabbitMQService struct {
	Managers map[string]*RabbitMQManager
}

func NewService(cfg config.RabbitMQ) (*RabbitMQService, error) {
	managers := map[string]*RabbitMQManager{}
	for context, node := range cfg.Nodes {
		if !node.Enabled {
			log.Infof("No RabbitMQ manager for context %s", context)
			continue
		}

		manager, err := buildManager(node, cfg.Exchanges)
		if err != nil {
			log.Errorf("Error while building RabbitMQ manager: %v", err)
			return nil, err
		}

		managers[context] = manager
	}

	return &RabbitMQService{Managers: managers}, nil
}

func buildManager(node config.RabbitMQNode, exchangesCfg []config.RabbitExchange) (*RabbitMQManager, error) {
	connection, err := BuildConnection(node)
	if err != nil {
		return nil, err
	}

	exchanges := BuildExchangeSpecs(exchangesCfg)

	return NewRabbitMQManager(connection, exchanges), nil
}

// Publish sends req.Payload as a JSON message to req.Exchange using req.RoutingKey.
//
// Default behavior:
//   - the message is published on the RabbitMQ node selected by req.ContextName,
//     falling back to the "default" context when no exact match exists;
//   - the payload is JSON-encoded and published with Content-Type
//     "application/json";
//   - the message is persistent unless req.DeliveryMode overrides it;
//   - the publish uses the AMQP mandatory flag by default, so unroutable
//     messages are returned as errors instead of being silently dropped.
//
// Failure modes:
//   - ErrManagerNotFound if no RabbitMQ manager exists for req.ContextName and
//     no "default" manager is configured;
//   - a validation error if Exchange or Payload is missing;
//   - PublishReturnedError if the exchange exists but no queue binding matches
//     the routing key and UnroutableOK is false;
//   - PublishNackedError if the broker negatively acknowledges the publish;
//   - a wrapped transport/protocol error if the connection/channel cannot be
//     opened or if the broker rejects the publish, for example because the
//     target exchange does not exist.
//
// Note that "no active consumer" is not an error by itself: as long as a queue
// is bound, RabbitMQ accepts the message and stores it until a consumer handles
// it.
func (s *RabbitMQService) Publish(ctx context.Context, req PublishRequest) error {
	manager, err := s.managerForContext(req.ContextName)
	if err != nil {
		return err
	}

	return manager.Publish(ctx, req)
}

func (s *RabbitMQService) managerForContext(contextName string) (*RabbitMQManager, error) {
	manager, ok := s.Managers[contextName]
	if !ok {
		manager, ok = s.Managers["default"]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrManagerNotFound, contextName)
		}
	}
	return manager, nil
}

// StartManagers runs the managers in background and returns Shutdowners
func (s *RabbitMQService) StartManagers() (managers []*RabbitMQManager, err error) {
	log.Info("Starting RabbitMQ managers")

	for c, m := range s.Managers {
		log.Infof("Starting RabbitMQ manager for context %s", c)

		started, err := m.Start(context.Background())
		if err == nil {
			managers = append(managers, started)
		} else {
			log.Errorf("Failed to start RabbitMQ manager for context %s: %v", c, err)
			break
		}
	}

	return
}
