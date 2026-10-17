package rabbitmq

import (
	"time"

	"github.com/gofrs/uuid/v5"
)

// CloudEvent is the envelope of the events the stack publishes on the
// activity exchange.
type CloudEvent struct {
	SpecVersion string    `json:"specversion"`
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Type        string    `json:"type"`
	Time        time.Time `json:"time"`
	TwakeOrg    string    `json:"twakeorg,omitempty"`
	TwakeActor  string    `json:"twakeactor,omitempty"`
	Data        any       `json:"data"`
}

// NewActivityRequest returns the request publishing a CloudEvent of this type
// on the activity exchange, with the type as routing key.
func NewActivityRequest(contextName, eventType, org string, data any) (PublishRequest, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return PublishRequest{}, err
	}
	return ActivityRequest(contextName, CloudEvent{
		ID:       id.String(),
		Type:     eventType,
		Time:     time.Now().UTC(),
		TwakeOrg: org,
		Data:     data,
	}), nil
}

// ActivityRequest returns the request publishing this event on the activity
// exchange, with its type as routing key.
func ActivityRequest(contextName string, event CloudEvent) PublishRequest {
	event.SpecVersion = "1.0"
	event.Source = "twake://drive"
	return PublishRequest{
		ContextName: contextName,
		Exchange:    ExchangeActivity,
		RoutingKey:  event.Type,
		Payload:     event,
		MessageID:   event.ID,
	}
}
