package limits

import (
	"errors"
	"time"

	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
)

// CounterType os an enum for the type of counters used by rate-limiting.
type CounterType int

// ErrRateLimitReached is the error returned when we were under the limit
// before the check, and reach the limit.
var ErrRateLimitReached = errors.New("Rate limit reached")

// ErrRateLimitExceeded is the error returned when the limit was already
// reached before the check.
var ErrRateLimitExceeded = errors.New("Rate limit exceeded")

const (
	// AuthType is used for counting the number of login attempts.
	AuthType CounterType = iota
	// TwoFactorGenerationType is used for counting the number of times a 2FA
	// is generated.
	TwoFactorGenerationType
	// TwoFactorType is used for counting the number of 2FA attempts.
	TwoFactorType
	// OAuthClientType is used for counting the number of OAuth clients.
	// creations/updates.
	OAuthClientType
	// SharingInviteType is used for counting the number of sharing invitations
	// sent to a given instance.
	SharingInviteType
	// SharingPublicLinkType is used for counting the number of public sharing
	// link consultations
	SharingPublicLinkType
	// JobThumbnailType is used for counting the number of thumbnail jobs
	// executed by an instance
	JobThumbnailType
	// JobShareTrackType is used for counting the number of updates of the
	// io.cozy.shared database
	JobShareTrackType
	// JobShareReplicateType is used for counting the number of replications
	JobShareReplicateType
	// JobShareUploadType is used for counting the file uploads
	JobShareUploadType
	// JobKonnectorType is used for counting the number of konnector executions
	JobKonnectorType
	// JobZipType is used for cozies exports
	JobZipType
	// JobSendMailType is used for mail sending
	JobSendMailType
	// JobServiceType is used for generic services
	// Ex: categorization or matching for banking
	JobServiceType
	// JobNotificationType is used for mobile notifications pushing
	JobNotificationType
	// SendHintByMail is used for sending the password hint by email
	SendHintByMail
	// JobNotesPersistType is used for saving notes to the VFS
	JobNotesPersistType
	// JobClientType is used for the jobs associated to a @client trigger
	JobClientType
	// ExportType is used for creating an export of the data
	ExportType
	// WebhookTriggerType is used for calling a webhook trigger
	WebhookTriggerType
	// JobCleanClientType is used for cleaning unused OAuth clients
	JobCleanClientType
	// ConfirmFlagshipType is used when the user is asked to manually certify
	// that an OAuth client is the flagship app.
	ConfirmFlagshipType
	// MagicLinkType is used when sending emails with a magic link that can
	// authenticate the user into a Cozy
	MagicLinkType
	// ResendOnboardingMailType is used for resending the onboarding link by email
	ResendOnboardingMailType
	// SharePasswordType is used for counting the password attempts on a
	// share by link protected by a password
	SharePasswordType
)

type counterConfig struct {
	Prefix string
	Limit  int64
	Period time.Duration
}

var configs = []counterConfig{
	// AuthType
	{
		Prefix: "auth",
		Limit:  1000,
		Period: 1 * time.Hour,
	},
	// TwoFactorGenerationType
	{
		Prefix: "two-factor-generation",
		Limit:  20,
		Period: 1 * time.Hour,
	},
	// TwoFactorType
	{
		Prefix: "two-factor",
		Limit:  10,
		Period: 5 * time.Minute,
	},
	// OAuthClientType
	{
		Prefix: "oauth-client",
		Limit:  50,
		Period: 1 * time.Hour,
	},
	// SharingInviteType
	{
		Prefix: "sharing-invite",
		Limit:  20,
		Period: 1 * time.Hour,
	},
	// SharingPublicLink
	{
		Prefix: "sharing-public-link",
		Limit:  2000,
		Period: 1 * time.Hour,
	},
	// JobThumbnail
	{
		Prefix: "job-thumbnail",
		Limit:  20000,
		Period: 1 * time.Hour,
	},
	// JobShareTrack
	{
		Prefix: "job-share-track",
		Limit:  20000,
		Period: 1 * time.Hour,
	},
	// JobShareReplicate
	{
		Prefix: "job-share-replicate",
		Limit:  2000,
		Period: 1 * time.Hour,
	},
	// JobShareUpload
	{
		Prefix: "job-share-upload",
		Limit:  1000,
		Period: 1 * time.Hour,
	},
	// JobKonnector
	{
		Prefix: "job-konnector",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// JobZip
	{
		Prefix: "job-zip",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// JobSendMail
	{
		Prefix: "job-sendmail",
		Limit:  200,
		Period: 1 * time.Hour,
	},
	// JobService
	{
		Prefix: "job-service",
		Limit:  200,
		Period: 1 * time.Hour,
	},
	// JobNotification
	{
		Prefix: "job-push",
		Limit:  30,
		Period: 1 * time.Hour,
	},
	// SendHintByMail
	{
		Prefix: "send-hint",
		Limit:  2,
		Period: 1 * time.Hour,
	},
	// JobNotesPersistType
	{
		Prefix: "job-notes-persist",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// JobClientType
	{
		Prefix: "job-client",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// ExportType
	{
		Prefix: "export",
		Limit:  5,
		Period: 24 * time.Hour,
	},
	// WebhookTriggerType
	{
		Prefix: "webhook-trigger",
		Limit:  30,
		Period: 1 * time.Hour,
	},
	// JobCleanClientType
	{
		Prefix: "job-clean-clients",
		Limit:  100,
		Period: 1 * time.Hour,
	},
	// ConfirmFlagshipType
	{
		Prefix: "confirm-flagship",
		Limit:  30,
		Period: 1 * time.Hour,
	},
	// MagicLinkType
	{
		Prefix: "magic-link",
		Limit:  30,
		Period: 1 * time.Hour,
	},
	// ResendOnboardingMailType
	{
		Prefix: "resend-onboarding-mail",
		Limit:  2,
		Period: 1 * time.Hour,
	},
	// SharePasswordType
	{
		Prefix: "share-password",
		Limit:  10,
		Period: 15 * time.Minute,
	},
}

// Counter is an interface for counting number of attempts that can be used to
// rate limit the number of logins and 2FA tries, and thus block bruteforce
// attacks.
type Counter interface {
	Increment(key string, timeLimit time.Duration) (int64, error)
	Reset(key string) error
}

// RateLimiter allow to rate limite the access to some resource.
type RateLimiter struct {
	counter Counter
}

// NewRateLimiter instantiate a new [RateLimiter].
//
// The backend selection is done based on the `client` argument. If a client is
// given, the redis backend is chosen, if nil is provided the inmemory backend would
// be chosen.
func NewRateLimiter(client redis.UniversalClient) *RateLimiter {
	if client == nil {
		return &RateLimiter{NewInMemory()}
	}

	return &RateLimiter{NewRedis(client)}
}

// CheckRateLimit returns an error if the counter for the given type and
// instance has reached the limit.
func (r *RateLimiter) CheckRateLimit(p prefixer.Prefixer, ct CounterType) error {
	return r.CheckRateLimitKey(p.DomainName(), ct)
}

// CheckRateLimitKey allows to check the rate-limit for a key
func (r *RateLimiter) CheckRateLimitKey(customKey string, ct CounterType) error {
	cfg := configs[ct]
	key := cfg.Prefix + ":" + customKey

	val, err := r.counter.Increment(key, cfg.Period)
	if err != nil {
		return err
	}

	// The first time we reach the limit, we provide a specific error message.
	// This allows to log a warning only once if needed.
	if val == cfg.Limit+1 {
		return ErrRateLimitReached
	}

	if val > cfg.Limit {
		return ErrRateLimitExceeded
	}

	return nil
}

// ResetCounter sets again to zero the counter for the given type and instance.
func (r *RateLimiter) ResetCounter(p prefixer.Prefixer, ct CounterType) {
	r.ResetCounterKey(p.DomainName(), ct)
}

// ResetCounterKey sets again to zero the counter for the given type and
// custom key.
func (r *RateLimiter) ResetCounterKey(customKey string, ct CounterType) {
	_ = r.counter.Reset(configs[ct].Prefix + ":" + customKey)
}

// IsLimitReachedOrExceeded return true if the limit has been reached or
// exceeded, false otherwise.
func IsLimitReachedOrExceeded(err error) bool {
	return errors.Is(err, ErrRateLimitReached) || errors.Is(err, ErrRateLimitExceeded)
}

// GetMaximumLimit returns the limit of a CounterType
func GetMaximumLimit(ct CounterType) int64 {
	return configs[ct].Limit
}

// SetMaximumLimit sets a new limit for a CounterType
func SetMaximumLimit(ct CounterType, newLimit int64) {
	configs[ct].Limit = newLimit
}
