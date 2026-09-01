package rabbitmq

const (
	ExchangeAuth      = "auth"
	ExchangeBilling   = "billing"
	ExchangeB2B       = "b2b"
	ExchangeMigration = "migration"
)

const (
	QueueUserPasswordUpdated       = "stack.user.password.updated"
	QueueUserCreated               = "stack.user.created"
	QueueUserPhoneUpdated          = "stack.user.phone.updated"
	QueueDomainSubscriptionChanged = "stack.domain.subscription.changed"
	QueueUser2FAUpdated            = "stack.user.2fa.updated"
	QueueUserRecoveryEmailUpdated  = "stack.user.recovery-email.updated"
	QueueB2BUserDeleted            = "stack.b2b.user.deleted"
	QueueB2BGroupLifecycle         = "stack.b2b.group.lifecycle"
	QueueAppCommands               = "stack.app.commands.queue"
	QueueBillingLifecycle          = "stack.billing.lifecycle"
)

const (
	RoutingKeyUserPasswordUpdated         = "user.password.updated"
	RoutingKeyB2BUserDeleted              = "domain.user.deleted"
	RoutingKeyB2BGroupCreated             = "b2b.group.created"
	RoutingKeyB2BGroupUpdated             = "b2b.group.updated"
	RoutingKeyB2BGroupDeleted             = "b2b.group.deleted"
	RoutingKeyB2BGroupMemberAdded         = "b2b.group.member.added"
	RoutingKeyB2BGroupMemberRemoved       = "b2b.group.member.removed"
	RoutingKeyUserDeletionRequested       = "user.deletion.requested"
	RoutingKeyNextcloudMigrationRequested = "nextcloud.migration.requested"
	RoutingKeyNextcloudMigrationCanceled  = "nextcloud.migration.canceled"
	RoutingKeyPaymentFailed               = "payment.failed"
	RoutingKeyPaymentRecovered            = "payment.recovered"
)

// BillingLifecycleMessage is published by the Cloudery when a payment event
// changes what the user has to be told, not what they are allowed to do:
// access control stays with the Cloudery.
//
// Exactly one of Domain and WorkplaceFqdn is set. Domain is a B2B
// organization, and every instance under it gets the banner.
type BillingLifecycleMessage struct {
	Domain        string `json:"domain,omitempty"`
	WorkplaceFqdn string `json:"workplaceFqdn,omitempty"`

	// Status is the subscription status as Stripe reports it, verbatim, read
	// back from the live subscription rather than from the event snapshot.
	Status string `json:"status"`
	// AttemptCount is the invoice attempt_count, passed through untouched. It
	// returns to 1 when a new invoice opens.
	AttemptCount int `json:"attemptCount,omitempty"`
	// EventID is the Stripe event id, logged so a displayed banner can be
	// traced back to what produced it.
	EventID string `json:"eventId"`
	// Timestamp is the Stripe event's own created time, in epoch seconds.
	// Delivery is at-least-once and unordered, so this, not the arrival time,
	// decides which event wins.
	Timestamp int64 `json:"timestamp"`
}

// UserDeletionRequestedMessage is published when a user asks Twake to delete the account linked to the current cozy instance.
type UserDeletionRequestedMessage struct {
	WorkplaceFqdn string `json:"workplaceFqdn"`
	Reason        string `json:"reason"`
	RequestedBy   string `json:"requestedBy"`
	RequestedAt   int64  `json:"requestedAt"`
}

// NextcloudMigrationRequestedMessage is published when a user starts a
// Nextcloud to Cozy migration from the Settings UI. The external migration
// service consumes it, fetches an app audience token from the Cloudery, and
// orchestrates the transfer through the Stack's Nextcloud routes.
//
// Credentials for the Nextcloud account are stored in the io.cozy.accounts
// document referenced by AccountID. They MUST NOT be included in this
// message: the broker is not a trust boundary for secrets.
type NextcloudMigrationRequestedMessage struct {
	MigrationID   string `json:"migrationId"`
	WorkplaceFqdn string `json:"workplaceFqdn"`
	AccountID     string `json:"accountId"`
	SourcePath    string `json:"sourcePath,omitempty"`
	Timestamp     int64  `json:"timestamp"`
}

// NextcloudMigrationCanceledMessage is published when a user requests
// cancellation of an in-flight Nextcloud migration. The Stack does not
// touch the tracking document for cancel — the migration service owns
// the terminal state so there is a single writer.
type NextcloudMigrationCanceledMessage struct {
	MigrationID   string `json:"migrationId"`
	WorkplaceFqdn string `json:"workplaceFqdn"`
	Timestamp     int64  `json:"timestamp"`
}
