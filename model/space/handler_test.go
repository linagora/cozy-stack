package space

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

type fakePublisher struct {
	requests []rabbitmq.PublishRequest
	err      error
}

func (p *fakePublisher) Publish(_ context.Context, req rabbitmq.PublishRequest) error {
	if p.err != nil {
		return p.err
	}
	p.requests = append(p.requests, req)
	return nil
}

func TestSpaceQueueUsesTheSpaceHandler(t *testing.T) {
	pub := &fakePublisher{}
	specs := rabbitmq.BuildExchangeSpecs([]config.RabbitExchange{{
		Name:   rabbitmq.ExchangeSpace,
		Kind:   "topic",
		Queues: []config.RabbitQueue{{Name: rabbitmq.QueueSpaceLifecycle, Bindings: []string{rabbitmq.RoutingKeySpaceCreated}}},
	}}, pub)

	require.Len(t, specs, 1)
	require.Len(t, specs[0].Queues, 1)
	require.Equal(t, NewHandler(pub), specs[0].Queues[0].Handler)
}

func TestSpaceHandler(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	created := func(t *testing.T, orgID, spaceID string) amqp.Delivery {
		t.Helper()
		body, err := json.Marshal(map[string]interface{}{
			"organizationId":     orgID,
			"organizationDomain": orgID + ".example",
			"id":                 spaceID,
			"name":               "Design Sprint",
			"members":            []interface{}{},
			"actor":              "admin@" + orgID + ".example",
			"timestamp":          "2026-10-05T09:12:44.512Z",
		})
		require.NoError(t, err)
		return amqp.Delivery{RoutingKey: "twake.space.created", Body: body}
	}

	t.Run("ProvisionsTheDriveAndPublishesItsID", func(t *testing.T) {
		org := newOrgInstance(t)
		spaceID := "3b9e2c71-" + strings.ToLower(utils.RandomString(8))
		pub := &fakePublisher{}
		h := NewHandler(pub)

		require.NoError(t, h.Handle(context.Background(), created(t, org.OrgID, spaceID)))
		require.NoError(t, h.Handle(context.Background(), created(t, org.OrgID, spaceID)))

		drives, err := sharing.ListDrives(org)
		require.NoError(t, err)
		require.Len(t, drives, 1)

		require.Len(t, pub.requests, 2)
		var ids []string
		for _, req := range pub.requests {
			require.Equal(t, "activity", req.Exchange)
			require.Equal(t, "com.twake.drive.space.provisioned.v1", req.RoutingKey)
			raw, err := json.Marshal(req.Payload)
			require.NoError(t, err)
			var event struct {
				SpecVersion string `json:"specversion"`
				ID          string `json:"id"`
				Source      string `json:"source"`
				Type        string `json:"type"`
				Time        string `json:"time"`
				TwakeOrg    string `json:"twakeorg"`
				Data        struct {
					SpaceID  string `json:"space_id"`
					Resource struct {
						Kind string `json:"kind"`
						ID   string `json:"id"`
					} `json:"resource"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(raw, &event))
			require.Equal(t, "1.0", event.SpecVersion)
			require.NotEmpty(t, event.ID)
			require.Equal(t, "twake://drive", event.Source)
			require.Equal(t, "com.twake.drive.space.provisioned.v1", event.Type)
			require.NotEmpty(t, event.Time)
			require.Equal(t, org.OrgID, event.TwakeOrg)
			require.Equal(t, spaceID, event.Data.SpaceID)
			require.Equal(t, "drive", event.Data.Resource.Kind)
			require.Equal(t, drives[0].SID, event.Data.Resource.ID)
			ids = append(ids, event.ID)
		}
		require.NotEqual(t, ids[0], ids[1])
	})

	t.Run("FailsWithoutAnOrganizationInstance", func(t *testing.T) {
		pub := &fakePublisher{}
		h := NewHandler(pub)

		err := h.Handle(context.Background(), created(t, "no-such-org-"+strings.ToLower(utils.RandomString(8)), "space-1"))
		require.Error(t, err)
		require.Empty(t, pub.requests)
	})

	t.Run("FailsOnAnIncompleteMessage", func(t *testing.T) {
		h := NewHandler(&fakePublisher{})
		for _, body := range []string{
			`not json`,
			`{"id":"space-1","name":"Design"}`,
			`{"organizationId":"acme","name":"Design"}`,
			`{"organizationId":"acme","id":"space-1"}`,
		} {
			err := h.Handle(context.Background(), amqp.Delivery{RoutingKey: "twake.space.created", Body: []byte(body)})
			require.Error(t, err, body)
		}
	})

	t.Run("FailsWhenThePublishFails", func(t *testing.T) {
		org := newOrgInstance(t)
		h := NewHandler(&fakePublisher{err: errors.New("broker down")})

		err := h.Handle(context.Background(), created(t, org.OrgID, "space-"+strings.ToLower(utils.RandomString(8))))
		require.Error(t, err)
	})
}
