package rabbitmq

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/crypto"
)

const DefaultDomain = "twake.app"

// PasswordChangeHandler handles password change messages.
type PasswordChangeHandler struct{}

// NewPasswordChangeHandler creates a new password change handler.
func NewPasswordChangeHandler() *PasswordChangeHandler {
	return &PasswordChangeHandler{}
}

// PasswordChangeMessage represents a password change message using the new schema.
type PasswordChangeMessage struct {
	TwakeID       string `json:"twakeId"`       // external user identifier
	Iterations    int    `json:"iterations"`    // PBKDF2 iterations used client-side (when applicable)
	Hash          string `json:"hash"`          // client-side hashed passphrase (base64)
	PublicKey     string `json:"publicKey"`     // [OPTIONAL] Bitwarden public key (base64)
	PrivateKey    string `json:"privateKey"`    // [OPTIONAL] Bitwarden private key (encrypted, CipherString)
	Key           string `json:"key"`           // [OPTIONAL] encrypted symmetric key (CipherString)
	Timestamp     int64  `json:"timestamp"`     // [OPTIONAL] unix timestamp of the event
	Domain        string `json:"domain"`        // [OPTIONAL] domain of the instance, e.g. "twake.app"
	WorkplaceFqdn string `json:"workplaceFqdn"` // The fully qualified workplace domain
}

// Handle processes a password change message.
func (h *PasswordChangeHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("password change: received password change message: %s", d.RoutingKey)
	log.Debugf("password change: message details - MessageId: %s, ContentType: %s, Body size: %d bytes", d.MessageId, d.ContentType, len(d.Body))

	var msg PasswordChangeMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("password change: failed to unmarshal password change message: %w", err)
	}
	log.Debugf("password change: successfully unmarshaled message for TwakeID: %s", msg.TwakeID)

	// Log incoming message with masked sensitive data
	log.Infof("password change: processing message - TwakeID: %s, Domain: %s, Iterations: %d, Hash: %s, PublicKey: %s, PrivateKey: %s, Key: %s, Timestamp: %d",
		msg.TwakeID,
		msg.Domain,
		msg.Iterations,
		maskSensitiveData(msg.Hash),
		maskSensitiveData(msg.PublicKey),
		maskSensitiveData(msg.PrivateKey),
		maskSensitiveData(msg.Key),
		msg.Timestamp)

	if msg.Domain == "" {
		log.Debugf("password change: no domain provided, using default: %s", DefaultDomain)
		msg.Domain = DefaultDomain
	}
	if msg.Hash == "" {
		log.Errorf("password change: validation failed - missing passphrase hash for TwakeID: %s", msg.TwakeID)
		return fmt.Errorf("password change: missing passphrase hash")
	}
	if msg.WorkplaceFqdn == "" {
		log.Errorf("password change: validation failed - missing workplaceFqdn for TwakeID: %s", msg.TwakeID)
		return fmt.Errorf("password change: missing workplaceFqdn")
	}
	if msg.Iterations <= 0 {
		return fmt.Errorf("password change: missing iterations")
	}
	log.Debugf("password change: message validation passed for TwakeID: %s", msg.TwakeID)

	decoded, err := decodePassword(msg.Hash)
	if err != nil {
		return err
	}
	params := lifecycle.PassParameters{
		Pass:       decoded,
		Iterations: msg.Iterations,
	}

	if msg.Key != "" {
		log.Debugf("password change: setting key parameter for TwakeID: %s", msg.TwakeID)
		params.Key = msg.Key
	}

	// if one of the keys is missing, do not update any of the keys
	if msg.PublicKey != "" && msg.PrivateKey != "" {
		log.Debugf("password change: setting public/private key parameters for TwakeID: %s", msg.TwakeID)
		params.PublicKey = msg.PublicKey
		params.PrivateKey = msg.PrivateKey
	} else {
		log.Debugf("password change: skipping key parameters (incomplete pair) for TwakeID: %s", msg.TwakeID)
	}

	log.Debugf("password change: retrieving instance for domain: %s", msg.WorkplaceFqdn)
	inst, err := lifecycle.GetInstance(msg.WorkplaceFqdn)
	if err != nil {
		return fmt.Errorf("password change: get instance: %w", err)
	}

	if err := lifecycle.ForceUpdatePassphraseWithSHash(inst, params.Pass, params); err != nil {
		return fmt.Errorf("password change: update passphrase: %w", err)
	}
	log.Infof("password change: successfully updated passphrase for instance: %s", inst.Domain)
	return nil
}

// UserCreatedHandler handles user creation messages.
type UserCreatedHandler struct{}

// NewUserCreatedHandler creates a new user created handler.
func NewUserCreatedHandler() *UserCreatedHandler {
	return &UserCreatedHandler{}
}

// UserCreatedMessage represents a user creation message.
type UserCreatedMessage struct {
	TwakeID            string `json:"twakeId"`
	Domain             string `json:"domain"`
	Mobile             string `json:"mobile"`
	InternalEmail      string `json:"internalEmail"`
	Iterations         int    `json:"iterations"`
	Hash               string `json:"hash"`
	PublicKey          string `json:"publicKey"`
	PrivateKey         string `json:"privateKey"`
	Key                string `json:"key"`
	Timestamp          int64  `json:"timestamp"`
	WorkplaceFqdn      string `json:"workplaceFqdn"` // The fully qualified workplace domain
	OrganizationID     string `json:"organizationId,omitempty"`
	OrganizationDomain string `json:"organizationDomain,omitempty"`
}

// Handle processes a user created message.
func (h *UserCreatedHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("user.created: received message: %s", d.RoutingKey)
	log.Debugf("user.created: message details - MessageId: %s, ContentType: %s, Body size: %d bytes", d.MessageId, d.ContentType, len(d.Body))

	var msg UserCreatedMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("user.created: failed to unmarshal message: %w", err)
	}
	log.Debugf("user.created: successfully unmarshaled message for TwakeID: %s", msg.TwakeID)

	// Log incoming message with masked sensitive data
	log.Infof("user.created: processing message - TwakeID: %s, Domain: %s, Mobile: %s, InternalEmail: %s, Iterations: %d, Hash: %s, PublicKey: %s, PrivateKey: %s, Key: %s, Timestamp: %d",
		msg.TwakeID,
		msg.Domain,
		msg.Mobile,
		msg.InternalEmail,
		msg.Iterations,
		maskSensitiveData(msg.Hash),
		maskSensitiveData(msg.PublicKey),
		maskSensitiveData(msg.PrivateKey),
		maskSensitiveData(msg.Key),
		msg.Timestamp)

	// Basic validation
	if msg.TwakeID == "" {
		return fmt.Errorf("user.created: missing twakeId")
	}

	if msg.Domain == "" {
		log.Debugf("user.created: no domain provided, using default: %s", DefaultDomain)
		msg.Domain = DefaultDomain
	}
	if msg.WorkplaceFqdn == "" {
		return fmt.Errorf("user.created: missing workplaceFqdn")
	}

	log.Debugf("user.created: looking for instance for domain: %s", msg.WorkplaceFqdn)
	inst, err := lifecycle.GetInstance(msg.WorkplaceFqdn)
	if err != nil {
		return fmt.Errorf("user.created: get instance: %w", err)
	}
	log.Debugf("user.created: message validation passed for TwakeID: %s", msg.TwakeID)

	if msg.Hash == "" {
		if !inst.HasForcedOIDC() {
			return fmt.Errorf("user.created: missing passphrase hash")
		}
		log.Infof("user.created: skipping passphrase update for instance %s (forced OIDC context: %s)", inst.Domain, inst.ContextName)
	} else {
		if msg.Iterations <= 0 {
			return fmt.Errorf("user.created: missing iterations")
		}
		decoded, err := decodePassword(msg.Hash)
		if err != nil {
			return err
		}

		params := lifecycle.PassParameters{
			Pass:       decoded,
			Iterations: msg.Iterations,
		}

		if msg.Key != "" {
			log.Debugf("user.created: setting key parameter for TwakeID: %s", msg.TwakeID)
			params.Key = msg.Key
		}

		// if one of the keys is missing, do not update any of the keys
		if msg.PublicKey != "" && msg.PrivateKey != "" {
			log.Debugf("user.created: setting public/private key parameters for TwakeID: %s", msg.TwakeID)
			params.PublicKey = msg.PublicKey
			params.PrivateKey = msg.PrivateKey
		} else {
			log.Debugf("user.created: skipping key parameters (incomplete pair) for TwakeID: %s", msg.TwakeID)
		}

		if err := lifecycle.ForceUpdatePassphraseWithSHash(inst, params.Pass, params); err != nil {
			return fmt.Errorf("user.created: update passphrase: %w", err)
		}
		log.Infof("user.created: successfully updated passphrase for instance: %s (PasswordDefined: %v)", inst.Domain, inst.PasswordDefined)
	}

	if strings.TrimSpace(msg.OrganizationID) != "" || strings.TrimSpace(msg.OrganizationDomain) != "" {
		scope, err := orgdirectory.ResolveOrganizationInstances(msg.OrganizationID, msg.OrganizationDomain)
		if err != nil {
			err = fmt.Errorf("user.created: %w", err)
			log.Errorf("user.created: failed to resolve organization instances for %s: %v", msg.WorkplaceFqdn, err)
			return err
		}
		if err := syncCreatedOrgContact(ctx, inst, msg, scope); err != nil {
			log.Errorf("user.created: failed to sync organization contacts for %s: %v", msg.WorkplaceFqdn, err)
			return err
		}
		if scope.OrganizationID != "" {
			if err := orgdirectory.CopyOrgDirectoryFromOrgInstance(ctx, inst, scope.OrganizationID); err != nil {
				log.Errorf("user.created: failed to copy organization directory for %s: %v", msg.WorkplaceFqdn, err)
				return err
			}
		}
	}

	return nil
}

// UserDeletedHandler handles B2B user deletion messages.
type UserDeletedHandler struct{}

// NewUserDeletedHandler creates a new user deleted handler.
func NewUserDeletedHandler() *UserDeletedHandler {
	return &UserDeletedHandler{}
}

// UserDeletedMessage represents a B2B user deletion message.
type UserDeletedMessage struct {
	Emitter        string `json:"emitter"`
	Type           string `json:"type"`
	WorkplaceFqdn  string `json:"workplaceFqdn"`
	InternalEmail  string `json:"internalEmail"`
	Reason         string `json:"reason"`
	OrganizationID string `json:"organizationId"`
	UserID         string `json:"userId"`
	Domain         string `json:"domain"`
}

// Handle processes a B2B user deletion message.
func (h *UserDeletedHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("user.deleted: received message: %s", d.RoutingKey)
	log.Debugf("user.deleted: message details - MessageId: %s, ContentType: %s, Body size: %d bytes",
		d.MessageId, d.ContentType, len(d.Body))

	var msg UserDeletedMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("user.deleted: failed to unmarshal message: %w", err)
	}

	msg.WorkplaceFqdn = strings.TrimSpace(msg.WorkplaceFqdn)
	msg.InternalEmail = strings.TrimSpace(msg.InternalEmail)
	msg.Domain = strings.TrimSpace(msg.Domain)

	if msg.WorkplaceFqdn == "" {
		return fmt.Errorf("user.deleted: missing workplaceFqdn")
	}
	if msg.InternalEmail == "" {
		return fmt.Errorf("user.deleted: missing internalEmail")
	}

	log.Infof("user.deleted: processing message - UserID: %s, InternalEmail: %s, WorkplaceFqdn: %s, Domain: %s, OrganizationID: %s, Reason: %s",
		msg.UserID, msg.InternalEmail, msg.WorkplaceFqdn, msg.Domain, msg.OrganizationID, msg.Reason)

	if err := SyncDeletedOrgContact(ctx, msg); err != nil {
		log.Errorf("user.deleted: failed to sync organization contacts for %s: %v", msg.WorkplaceFqdn, err)
		return err
	}

	return nil
}

// B2BGroupLifecycleHandler handles B2B group lifecycle and membership events.
type B2BGroupLifecycleHandler struct{}

// NewB2BGroupLifecycleHandler creates a new B2B group lifecycle handler.
func NewB2BGroupLifecycleHandler() *B2BGroupLifecycleHandler {
	return &B2BGroupLifecycleHandler{}
}

// Handle processes B2B group lifecycle messages.
func (h *B2BGroupLifecycleHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("b2b.group: received message: %s", d.RoutingKey)
	log.Debugf("b2b.group: message details - MessageId: %s, ContentType: %s, Body size: %d bytes",
		d.MessageId, d.ContentType, len(d.Body))

	switch d.RoutingKey {
	case RoutingKeyB2BGroupCreated:
		var msg orgdirectory.GroupCreatedMessage
		if err := json.Unmarshal(d.Body, &msg); err != nil {
			return fmt.Errorf("b2b.group.created: failed to unmarshal message: %w", err)
		}
		return orgdirectory.SyncGroupCreated(ctx, msg)
	case RoutingKeyB2BGroupUpdated:
		var msg orgdirectory.GroupUpdatedMessage
		if err := json.Unmarshal(d.Body, &msg); err != nil {
			return fmt.Errorf("b2b.group.updated: failed to unmarshal message: %w", err)
		}
		return orgdirectory.SyncGroupUpdated(ctx, msg)
	case RoutingKeyB2BGroupDeleted:
		var msg orgdirectory.GroupDeletedMessage
		if err := json.Unmarshal(d.Body, &msg); err != nil {
			return fmt.Errorf("b2b.group.deleted: failed to unmarshal message: %w", err)
		}
		return orgdirectory.SyncGroupDeleted(ctx, msg)
	case RoutingKeyB2BGroupMemberAdded:
		var msg orgdirectory.GroupMembersMessage
		if err := json.Unmarshal(d.Body, &msg); err != nil {
			return fmt.Errorf("b2b.group.member.added: failed to unmarshal message: %w", err)
		}
		return orgdirectory.SyncGroupMembersAdded(ctx, msg)
	case RoutingKeyB2BGroupMemberRemoved:
		var msg orgdirectory.GroupMembersMessage
		if err := json.Unmarshal(d.Body, &msg); err != nil {
			return fmt.Errorf("b2b.group.member.removed: failed to unmarshal message: %w", err)
		}
		return orgdirectory.SyncGroupMembersRemoved(ctx, msg)
	default:
		return fmt.Errorf("b2b.group: unsupported routing key %s", d.RoutingKey)
	}
}

// UserPhoneUpdatedHandler handles user phone update messages.
type UserPhoneUpdatedHandler struct{}

// NewUserPhoneUpdatedHandler creates a new user phone update handler.
func NewUserPhoneUpdatedHandler() *UserPhoneUpdatedHandler {
	return &UserPhoneUpdatedHandler{}
}

// UserPhoneUpdatedMessage represents a user phone update message.
type UserPhoneUpdatedMessage struct {
	TwakeID       string `json:"twakeId"`
	Mobile        string `json:"mobile"`
	InternalEmail string `json:"internalEmail"`
	WorkplaceFqdn string `json:"workplaceFqdn"`
}

// Handle processes a user phone update message.
func (h *UserPhoneUpdatedHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("user.phone.updated: received message: %s", d.RoutingKey)
	log.Debugf("user.phone.updated: message details - MessageId: %s, ContentType: %s, Body size: %d bytes",
		d.MessageId, d.ContentType, len(d.Body))

	var msg UserPhoneUpdatedMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("user.phone.updated: failed to unmarshal message: %w", err)
	}

	log.Infof("user.phone.updated: processing message - TwakeID: %s, Mobile: %s, InternalEmail: %s, WorkplaceFqdn: %s",
		msg.TwakeID, msg.Mobile, msg.InternalEmail, msg.WorkplaceFqdn)

	if msg.TwakeID == "" {
		return fmt.Errorf("user.phone.updated: missing twakeId")
	}
	if msg.Mobile == "" {
		return fmt.Errorf("user.phone.updated: missing mobile")
	}
	if msg.WorkplaceFqdn == "" {
		return fmt.Errorf("user.phone.updated: missing workplaceFqdn")
	}

	inst, err := lifecycle.GetInstance(msg.WorkplaceFqdn)
	if err != nil {
		return fmt.Errorf("user.phone.updated: get instance: %w", err)
	}

	// Get current settings document
	settings, err := inst.SettingsDocument()
	if err != nil {
		return fmt.Errorf("user.phone.updated: get settings document: %w", err)
	}

	// Update phone in settings
	settings.M["phone"] = msg.Mobile

	if err := lifecycle.Patch(inst, &lifecycle.Options{
		SettingsObj:  settings,
		FromCloudery: true, // XXX: do not update the instance's phone on the Cloudery as its API does not permit it and the Cloudery only uses it when requesting the instance creation from the stack
	}); err != nil {
		return fmt.Errorf("user.phone.updated: update settings: %w", err)
	}

	log.Infof("user.phone.updated: successfully updated phone for instance: %s", inst.Domain)
	return nil
}

// User2FAUpdatedHandler handles user 2FA toggle messages.
type User2FAUpdatedHandler struct{}

// NewUser2FAUpdatedHandler creates a new user 2FA update handler.
func NewUser2FAUpdatedHandler() *User2FAUpdatedHandler {
	return &User2FAUpdatedHandler{}
}

// User2FAUpdatedMessage represents a user 2FA toggle message.
type User2FAUpdatedMessage struct {
	TwoFactorEnabled bool   `json:"twoFactorEnabled"`
	WorkplaceFqdn    string `json:"workplaceFqdn"`
	InternalEmail    string `json:"internalEmail"`
}

// Handle processes a user 2FA toggle message.
func (h *User2FAUpdatedHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("user.2fa.updated: received message: %s", d.RoutingKey)
	log.Debugf("user.2fa.updated: message details - MessageId: %s, ContentType: %s, Body size: %d bytes",
		d.MessageId, d.ContentType, len(d.Body))

	var msg User2FAUpdatedMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("user.2fa.updated: failed to unmarshal message: %w", err)
	}

	log.Infof("user.2fa.updated: processing message - TwoFactorEnabled: %t, WorkplaceFqdn: %s, InternalEmail: %s",
		msg.TwoFactorEnabled, msg.WorkplaceFqdn, msg.InternalEmail)

	if msg.WorkplaceFqdn == "" {
		return fmt.Errorf("user.2fa.updated: missing workplaceFqdn")
	}

	inst, err := lifecycle.GetInstance(msg.WorkplaceFqdn)
	if err != nil {
		return fmt.Errorf("user.2fa.updated: get instance: %w", err)
	}

	var authMode instance.AuthMode
	if msg.TwoFactorEnabled {
		authMode = instance.TwoFactorOIDC
	}

	if err := lifecycle.UpdateAuthMode(inst, authMode); err != nil {
		return fmt.Errorf("user.2fa.updated: update auth mode: %w", err)
	}

	log.Infof("user.2fa.updated: successfully updated auth mode for instance: %s (TwoFactorEnabled: %t)", inst.Domain, msg.TwoFactorEnabled)
	return nil
}

// UserRecoveryEmailUpdatedHandler handles recovery email update messages.
type UserRecoveryEmailUpdatedHandler struct{}

// NewUserRecoveryEmailUpdatedHandler creates a new recovery email update handler.
func NewUserRecoveryEmailUpdatedHandler() *UserRecoveryEmailUpdatedHandler {
	return &UserRecoveryEmailUpdatedHandler{}
}

// UserRecoveryEmailUpdatedMessage represents a recovery email update message.
type UserRecoveryEmailUpdatedMessage struct {
	RecoveryEmail string `json:"recoveryEmail"`
	InternalEmail string `json:"internalEmail"`
	WorkplaceFqdn string `json:"workplaceFqdn"`
}

// Handle processes a recovery email update message.
func (h *UserRecoveryEmailUpdatedHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("user.recovery-email.updated: received message: %s", d.RoutingKey)
	log.Debugf("user.recovery-email.updated: message details - MessageId: %s, ContentType: %s, Body size: %d bytes",
		d.MessageId, d.ContentType, len(d.Body))

	var msg UserRecoveryEmailUpdatedMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("user.recovery-email.updated: failed to unmarshal message: %w", err)
	}

	log.Infof("user.recovery-email.updated: processing message - RecoveryEmail: %s, WorkplaceFqdn: %s, InternalEmail: %s",
		msg.RecoveryEmail, msg.WorkplaceFqdn, msg.InternalEmail)

	if msg.WorkplaceFqdn == "" {
		return fmt.Errorf("user.recovery-email.updated: missing workplaceFqdn")
	}

	inst, err := lifecycle.GetInstance(msg.WorkplaceFqdn)
	if err != nil {
		return fmt.Errorf("user.recovery-email.updated: get instance: %w", err)
	}

	settings, err := inst.SettingsDocument()
	if err != nil {
		return fmt.Errorf("user.recovery-email.updated: get settings document: %w", err)
	}

	// recovery_email is used by the Twake SaaS to display the recovery
	// email in the settings application.
	settings.M["recovery_email"] = msg.RecoveryEmail

	if err := lifecycle.Patch(inst, &lifecycle.Options{
		SettingsObj:  settings,
		FromCloudery: true,
	}); err != nil {
		return fmt.Errorf("user.recovery-email.updated: update settings: %w", err)
	}

	log.Infof("user.recovery-email.updated: successfully updated recovery email for instance: %s", inst.Domain)
	return nil
}

type DomainSubscriptionChangedHandler struct{}

func NewDomainSubscriptionChangedHandler() *DomainSubscriptionChangedHandler {
	return &DomainSubscriptionChangedHandler{}
}

type DomainSubscriptionChangedMessage struct {
	Domain     string               `json:"domain"`
	IsPaying   bool                 `json:"isPaying"`
	CanUpgrade bool                 `json:"canUpgrade"`
	Features   SubscriptionFeatures `json:"features"`
}

type SubscriptionFeatures struct {
	Stack SubscriptionStackFeatures `json:"stack"`
}

type SubscriptionStackFeatures struct {
	FeatureSets []string `json:"featureSets"`
	DiskQuota   int64    `json:"diskQuota"`
}

func (h *DomainSubscriptionChangedHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("domain.subscription.changed: received message: %s", d.RoutingKey)
	log.Debugf("domain.subscription.changed: message details - MessageId: %s, ContentType: %s, Body size: %d bytes",
		d.MessageId, d.ContentType, len(d.Body))

	var msg DomainSubscriptionChangedMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("domain.subscription.changed: failed to unmarshal message: %w", err)
	}

	log.Infof("domain.subscription.changed: processing message - Domain: %s, IsPaying: %t, CanUpgrade: %t, Features: %v",
		msg.Domain, msg.IsPaying, msg.CanUpgrade, msg.Features)

	if msg.Domain == "" {
		return fmt.Errorf("domain.subscription.changed: missing organization domain")
	}

	if msg.Features.Stack.FeatureSets == nil {
		return fmt.Errorf("domain.subscription.changed: missing feature sets for organization %s: %v", msg.Domain, msg.Features)
	}

	list, err := lifecycle.ListOrgInstances(msg.Domain)
	if err != nil {
		return fmt.Errorf("domain.subscription.changed: could not list instances for organization %s: %w", msg.Domain, err)
	}
	if len(list) == 0 {
		log.Infof("domain.subscription.changed: no instances found for organization %s", msg.Domain)
		return nil
	}

	for _, inst := range list {
		if err := lifecycle.Patch(inst, &lifecycle.Options{
			DiskQuota:    msg.Features.Stack.DiskQuota,
			FeatureSets:  msg.Features.Stack.FeatureSets,
			FromCloudery: true, // XXX: do not update the instance on the Cloudery as the message comes from the Cloudery
		}); err != nil {
			return fmt.Errorf("domain.subscription.changed: update instance %s: %w", inst.Domain, err)
		}
		log.Infof("domain.subscription.changed: successfully updated instance %s", inst.Domain)
	}

	log.Infof("domain.subscription.changed: successfully updated %d instances for organization %s", len(list), msg.Domain)

	return nil
}

// AppInstallHandler handles app installation and uninstallation messages.
type AppInstallHandler struct{}

// NewAppInstallHandler creates a new app install handler.
func NewAppInstallHandler() *AppInstallHandler {
	return &AppInstallHandler{}
}

// AppInstallMessage represents an app installation/uninstallation message.
type AppInstallMessage struct {
	Emitter       string `json:"emitter"`       // the application requesting the operation
	Type          string `json:"type"`          // "app.install" | "app.uninstall"
	WorkplaceFqdn string `json:"workplaceFqdn"` // the instance FQDN
	InternalEmail string `json:"internalEmail"` // internal email (optional)
	Reason        string `json:"reason"`        // why this operation is performed
	Slug          string `json:"slug"`          // the application slug
	Source        string `json:"source"`        // registry source URL (e.g. "registry://admin/stable")
	Timestamp     int64  `json:"timestamp"`     // when
}

// Handle processes an app installation/uninstallation message.
func (h *AppInstallHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	log.Infof("app.install: received message: %s", d.RoutingKey)
	log.Debugf("app.install: message details - MessageId: %s, ContentType: %s, Body size: %d bytes",
		d.MessageId, d.ContentType, len(d.Body))

	var msg AppInstallMessage
	if err := json.Unmarshal(d.Body, &msg); err != nil {
		return fmt.Errorf("app.install: failed to unmarshal message: %w", err)
	}

	log.Infof("app.install: processing message - Emitter: %s, Type: %s, Slug: %s, WorkplaceFqdn: %s, Reason: %s, Source: %s, Timestamp: %d",
		msg.Emitter, msg.Type, msg.Slug, msg.WorkplaceFqdn, msg.Reason, msg.Source, msg.Timestamp)

	// Validation
	if msg.Emitter == "" {
		return fmt.Errorf("app.install: missing emitter")
	}
	if msg.Slug == "" {
		return fmt.Errorf("app.install: missing slug")
	}
	if msg.WorkplaceFqdn == "" {
		return fmt.Errorf("app.install: missing workplaceFqdn")
	}

	// Log the source application requesting the operation
	log.Infof("app.install: operation requested by emitter: %s", msg.Emitter)

	// Get the instance
	log.Debugf("app.install: retrieving instance for domain: %s", msg.WorkplaceFqdn)
	inst, err := lifecycle.GetInstance(msg.WorkplaceFqdn)
	if err != nil {
		return fmt.Errorf("app.install: get instance: %w", err)
	}

	// Process the action
	switch msg.Type {
	case "app.install":
		return h.handleInstall(inst, msg)
	case "app.uninstall":
		return h.handleUninstall(inst, msg)
	default:
		return fmt.Errorf("app.install: invalid type %s, must be 'app.install' or 'app.uninstall'", msg.Type)
	}
}

// handleInstall installs an application on the instance.
func (h *AppInstallHandler) handleInstall(inst *instance.Instance, msg AppInstallMessage) error {
	log.Infof("app.install: installing app %s on instance %s (reason: %s)", msg.Slug, msg.WorkplaceFqdn, msg.Reason)

	// Use provided source if available, otherwise construct default
	source := msg.Source
	if source == "" || !strings.HasPrefix(source, "registry://") {
		source = "registry://" + msg.Slug + "/stable"
	}

	// Try to install as webapp first, then as konnector if it fails due to type mismatch
	appTypes := []consts.AppType{consts.WebappType, consts.KonnectorType}
	var lastErr error

	for _, appType := range appTypes {
		installer, err := app.NewInstaller(inst, app.Copier(appType, inst), &app.InstallerOptions{
			Operation:  app.Install,
			Type:       appType,
			SourceURL:  source,
			Slug:       msg.Slug,
			Registries: inst.Registries(),
		})
		if err != nil {
			// If the app already exists, that's OK
			if errors.Is(err, app.ErrAlreadyExists) {
				log.Infof("app.install: app %s already exists on instance %s", msg.Slug, msg.WorkplaceFqdn)
				return nil
			}
			log.Debugf("app.install: failed to create installer for %s as %s: %v", msg.Slug, appType.String(), err)
			lastErr = err
			continue
		}

		_, err = installer.RunSync()
		if err != nil {
			// If the app already exists, that's OK
			if errors.Is(err, app.ErrAlreadyExists) {
				log.Infof("app.install: app %s already exists on instance %s", msg.Slug, msg.WorkplaceFqdn)
				return nil
			}
			// Only try the other app type if this one failed due to manifest type mismatch.
			// For any other error (filesystem, database, network, etc.), return immediately
			// to avoid hiding the real error with a misleading type mismatch error.
			if errors.Is(err, app.ErrInvalidManifestForWebapp) || errors.Is(err, app.ErrInvalidManifestForKonnector) {
				log.Debugf("app.install: manifest type mismatch for %s as %s, trying other type", msg.Slug, appType.String())
				lastErr = err
				continue
			}
			return fmt.Errorf("app.install: failed to install app %s on instance %s: %w", msg.Slug, msg.WorkplaceFqdn, err)
		}

		log.Infof("app.install: successfully installed app %s as %s on instance %s", msg.Slug, appType.String(), msg.WorkplaceFqdn)
		return nil
	}

	return fmt.Errorf("app.install: failed to install app %s on instance %s: %w", msg.Slug, msg.WorkplaceFqdn, lastErr)
}

// handleUninstall uninstalls an application from the instance.
func (h *AppInstallHandler) handleUninstall(inst *instance.Instance, msg AppInstallMessage) error {
	log.Infof("app.install: uninstalling app %s from instance %s (reason: %s)", msg.Slug, msg.WorkplaceFqdn, msg.Reason)

	// First, try to find the app to determine its type
	appTypes := []consts.AppType{consts.WebappType, consts.KonnectorType}
	var foundType consts.AppType
	var found bool

	for _, appType := range appTypes {
		_, err := app.GetBySlug(inst, msg.Slug, appType)
		if err == nil {
			foundType = appType
			found = true
			break
		}
	}

	if !found {
		log.Infof("app.install: app %s not found on instance %s, nothing to uninstall", msg.Slug, msg.WorkplaceFqdn)
		return nil // Not an error if the app doesn't exist
	}

	// Uninstall the app
	installer, err := app.NewInstaller(inst, app.Copier(foundType, inst), &app.InstallerOptions{
		Operation:  app.Delete,
		Type:       foundType,
		Slug:       msg.Slug,
		Registries: inst.Registries(),
	})
	if err != nil {
		return fmt.Errorf("app.install: failed to create uninstaller: %w", err)
	}

	_, err = installer.RunSync()
	if err != nil {
		return fmt.Errorf("app.install: failed to uninstall app %s: %w", msg.Slug, err)
	}

	log.Infof("app.install: successfully uninstalled app %s from instance %s", msg.Slug, msg.WorkplaceFqdn)
	return nil
}

func decodePassword(hash string) ([]byte, error) {
	// Decode base64 hash if applicable
	decoded, err := base64.StdEncoding.DecodeString(hash)
	if err != nil {
		return nil, fmt.Errorf("user.created: invalid passphrase hash format: %w", err)
	}

	// Validate the hash format using scrypt's exported validator (uses UnmarshalText internally)
	if err := crypto.ValidateScryptHashFormat(decoded); err != nil {
		return nil, fmt.Errorf("user.created: invalid passphrase hash format: %w", err)
	}
	return decoded, nil
}

func maskSensitiveData(data string) string {
	if data == "" {
		return ""
	}
	// Show first 3 and last 3 characters, mask the middle with asterisks
	if len(data) <= 8 {
		return strings.Repeat("*", len(data))
	}
	return data[:3] + strings.Repeat("*", len(data)-6) + data[len(data)-3:]
}
