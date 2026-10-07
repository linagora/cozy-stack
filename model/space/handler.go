package space

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/gofrs/uuid/v5"
	amqp "github.com/rabbitmq/amqp091-go"
)

func init() {
	rabbitmq.RegisterHandler(rabbitmq.QueueSpaceLifecycle, func(p rabbitmq.Publisher) rabbitmq.Handler {
		return NewHandler(p)
	})
}

var log = logger.WithNamespace("space")

// CreatedMessage is the payload of twake.space.created.
type CreatedMessage struct {
	OrganizationID     string    `json:"organizationId"`
	OrganizationDomain string    `json:"organizationDomain"`
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Members            []Member  `json:"members"`
	Actor              string    `json:"actor"`
	Timestamp          time.Time `json:"timestamp"`
}

// CloudEvent is the envelope of the events published on the activity
// exchange.
type CloudEvent struct {
	SpecVersion string    `json:"specversion"`
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Type        string    `json:"type"`
	Time        time.Time `json:"time"`
	TwakeOrg    string    `json:"twakeorg"`
	TwakeActor  string    `json:"twakeactor,omitempty"`
	Data        any       `json:"data"`
}

func activityRequest(eventType, org, actor string, data any) (*rabbitmq.PublishRequest, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	event := CloudEvent{
		SpecVersion: "1.0",
		ID:          id.String(),
		Source:      "twake://drive",
		Type:        eventType,
		Time:        time.Now().UTC(),
		TwakeOrg:    org,
		TwakeActor:  actor,
		Data:        data,
	}
	return &rabbitmq.PublishRequest{
		Exchange:   rabbitmq.ExchangeActivity,
		RoutingKey: eventType,
		Payload:    event,
		MessageID:  event.ID,
	}, nil
}

// ProvisionedData is the data of com.twake.drive.space.provisioned.v1.
type ProvisionedData struct {
	SpaceID  string   `json:"space_id"`
	Resource Resource `json:"resource"`
}

// Resource names the resource of a space in an app.
type Resource struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Handler provisions the shared drive of a space on the organization
// instance and announces it on the activity exchange.
type Handler struct {
	publisher rabbitmq.Publisher
}

// NewHandler creates a space handler publishing with the given publisher.
func NewHandler(publisher rabbitmq.Publisher) *Handler {
	return &Handler{publisher: publisher}
}

// Handle processes a space event.
func (h *Handler) Handle(ctx context.Context, d amqp.Delivery) error {
	if d.RoutingKey != rabbitmq.RoutingKeySpaceCreated {
		return fmt.Errorf("space: unsupported routing key %s", d.RoutingKey)
	}
	var msg CreatedMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("twake.space.created: failed to unmarshal message: %w", err)
	}
	msg.OrganizationID = strings.TrimSpace(msg.OrganizationID)
	msg.ID = strings.TrimSpace(msg.ID)
	switch {
	case msg.OrganizationID == "":
		return errors.New("twake.space.created: missing organizationId")
	case msg.ID == "":
		return errors.New("twake.space.created: missing id")
	case msg.Name == "":
		return errors.New("twake.space.created: missing name")
	}
	log.Infof("twake.space.created: space %s of organization %s with %d members", msg.ID, msg.OrganizationID, len(msg.Members))

	inst, err := orgdirectory.FindOrganizationInstance(ctx, msg.OrganizationID)
	if err != nil {
		return fmt.Errorf("twake.space.created: %w", err)
	}
	if inst == nil {
		return fmt.Errorf("twake.space.created: no organization instance for %s", msg.OrganizationID)
	}
	if err := inst.MakeVFS(); err != nil {
		return fmt.Errorf("twake.space.created: %w", err)
	}

	for i := range msg.Members {
		msg.Members[i].Email = strings.TrimSpace(msg.Members[i].Email)
	}
	s, err := ProvisionDrive(inst, Space{
		ID:             msg.ID,
		OrganizationID: msg.OrganizationID,
		Name:           msg.Name,
		Members:        msg.Members,
		Timestamp:      msg.Timestamp,
	})
	if err != nil {
		return fmt.Errorf("twake.space.created: provision drive of space %s on %s: %w", msg.ID, inst.Domain, err)
	}

	req, err := activityRequest(rabbitmq.RoutingKeyDriveSpaceProvisioned, msg.OrganizationID, "", ProvisionedData{
		SpaceID:  msg.ID,
		Resource: Resource{Kind: "drive", ID: s.SID},
	})
	if err != nil {
		return err
	}
	req.ContextName = inst.ContextName
	if err := h.publisher.Publish(ctx, *req); err != nil {
		return fmt.Errorf("twake.space.created: publish drive of space %s: %w", msg.ID, err)
	}
	log.Infof("twake.space.created: drive %s of space %s published", s.SID, msg.ID)
	return nil
}
