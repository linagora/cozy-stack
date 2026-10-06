package rabbitmq

import (
	"context"
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

func TestWaitForPublishResultIgnoresClosedReturnChannel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	confirms := make(chan amqp.Confirmation, 1)
	confirms <- amqp.Confirmation{Ack: true}

	returns := make(chan amqp.Return)
	close(returns)

	err := waitForPublishResult(ctx, ExchangeAuth, RoutingKeyUserDeletionRequested, confirms, returns)
	require.NoError(t, err)
}

func TestWaitForPublishResultPrefersReturnedMessageWhenConfirmChannelCloses(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	confirms := make(chan amqp.Confirmation)
	close(confirms)

	returns := make(chan amqp.Return, 1)
	returns <- amqp.Return{ReplyCode: 312, ReplyText: "NO_ROUTE"}

	err := waitForPublishResult(ctx, ExchangeAuth, RoutingKeyUserDeletionRequested, confirms, returns)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPublishReturned)

	var returned *PublishReturnedError
	require.True(t, errors.As(err, &returned))
	require.Equal(t, uint16(312), returned.ReplyCode)
	require.Equal(t, "NO_ROUTE", returned.ReplyText)
}

func TestPublishRequestRoutingKey(t *testing.T) {
	t.Parallel()

	req := PublishRequest{Exchange: ExchangeAuth, Payload: struct{}{}}
	require.Error(t, req.validate())
	req.Fanout = true
	require.NoError(t, req.validate())
}
