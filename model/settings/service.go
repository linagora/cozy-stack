package settings

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/cloudery"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/token"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/emailer"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

const TokenExpiration = 7 * 24 * time.Hour

var (
	ErrInvalidType    = errors.New("invalid type")
	ErrInvalidID      = errors.New("invalid id")
	ErrNoPendingEmail = errors.New("no pending email")
)

type ExternalTies struct {
	HasBlockingSubscription bool                           `json:"has_blocking_subscription"`
	BlockingSubscription    *cloudery.BlockingSubscription `json:"blocking_subscription,omitempty"`
}

// Storage used to persiste and fetch settings data.
type Storage interface {
	setInstanceSettings(db prefixer.Prefixer, doc *couchdb.JSONDoc) error
	getInstanceSettings(db prefixer.Prefixer) (*couchdb.JSONDoc, error)
}

// SettingsService handle the business logic around "settings".
//
// This service handle 2 structured documents present in [consts.Settings]
// - The "instance settings" ([consts.InstanceSettingsID])
// - The "bitwarden settings" ([consts.BitwardenSettingsID]) (#TODO)
type SettingsService struct {
	emailer  emailer.Emailer
	instance instance.Service
	token    token.Service
	cloudery cloudery.Service
	storage  Storage
}

// NewService instantiates a new [SettingsService].
func NewService(
	emailer emailer.Emailer,
	instance instance.Service,
	token token.Service,
	cloudery cloudery.Service,
	storage Storage,
) *SettingsService {
	return &SettingsService{emailer, instance, token, cloudery, storage}
}

// PublicName returns the settings' public name or a default one if missing
func (s *SettingsService) PublicName(db prefixer.Prefixer) (string, error) {
	doc, err := s.storage.getInstanceSettings(db)
	if err != nil {
		return "", err
	}
	publicName, _ := doc.M["public_name"].(string)
	// if the public name is not defined, use the instance's domain
	if publicName == "" {
		split := strings.Split(db.DomainName(), ".")
		publicName = split[0]
	}
	return publicName, nil
}

// GetInstanceSettings allows for fetch directly the [consts.InstanceSettingsID] couchdb document.
func (s *SettingsService) GetInstanceSettings(db prefixer.Prefixer) (*couchdb.JSONDoc, error) {
	return s.storage.getInstanceSettings(db)
}

// SetInstanceSettings allows a set directly the [consts.InstanceSettingsID] couchdb document.
func (s *SettingsService) SetInstanceSettings(db prefixer.Prefixer, doc *couchdb.JSONDoc) error {
	return s.storage.setInstanceSettings(db, doc)
}

type UpdateEmailCmd struct {
	Passphrase []byte
	Email      string
}

// StartEmailUpdate will start the email updating process.
//
// This process consists of validating the user with a password and sending
// a validation email to the new address with a validation link. This link
// will allow the user to confirm its email.
func (s *SettingsService) StartEmailUpdate(inst *instance.Instance, cmd *UpdateEmailCmd) error {
	err := s.instance.CheckPassphrase(inst, cmd.Passphrase)
	if err != nil {
		return fmt.Errorf("failed to check passphrase: %w", err)
	}

	settings, err := s.storage.getInstanceSettings(inst)
	if err != nil {
		return fmt.Errorf("failed to fetch the settings: %w", err)
	}

	publicName, err := s.PublicName(inst)
	if err != nil {
		return fmt.Errorf("failed to retrieve the instance settings: %w", err)
	}

	settings.M["pending_email"] = cmd.Email

	token, err := s.token.GenerateAndSave(inst, token.EmailUpdate, cmd.Email, TokenExpiration)
	if err != nil {
		return fmt.Errorf("failed to generate and save the confirmation token: %w", err)
	}

	err = s.storage.setInstanceSettings(inst, settings)
	if err != nil {
		return fmt.Errorf("failed to save the settings changes: %w", err)
	}

	link := inst.PageURL("/settings/email/confirm", url.Values{
		"token": []string{token},
	})

	err = s.emailer.SendPendingEmail(inst, &emailer.TransactionalEmailCmd{
		TemplateName: "update_email",
		TemplateValues: map[string]interface{}{
			"PublicName":      publicName,
			"EmailUpdateLink": link,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to send the email: %w", err)
	}

	return nil
}

// ResendEmailUpdate will resend the validation email.
func (s *SettingsService) ResendEmailUpdate(inst *instance.Instance) error {
	settings, err := s.storage.getInstanceSettings(inst)
	if err != nil {
		return fmt.Errorf("failed to fetch the settings: %w", err)
	}

	publicName, err := s.PublicName(inst)
	if err != nil {
		return fmt.Errorf("failed to retrieve the instance settings: %w", err)
	}

	pendingEmail, ok := settings.M["pending_email"].(string)
	if !ok {
		return ErrNoPendingEmail
	}

	token, err := s.token.GenerateAndSave(inst, token.EmailUpdate, pendingEmail, TokenExpiration)
	if err != nil {
		return fmt.Errorf("failed to generate and save the confirmation token: %w", err)
	}

	link := inst.PageURL("/settings/email/confirm", url.Values{
		"token": []string{token},
	})

	err = s.emailer.SendPendingEmail(inst, &emailer.TransactionalEmailCmd{
		TemplateName: "update_email",
		TemplateValues: map[string]interface{}{
			"PublicName":      publicName,
			"EmailUpdateLink": link,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to send the email: %w", err)
	}

	return nil
}

// ConfirmEmailUpdate is the second step to the email update process.
//
// This step consiste to make the email change effectif and relay the change
// into the cloudery.
func (s *SettingsService) ConfirmEmailUpdate(inst *instance.Instance, tok string) error {
	settings, err := s.storage.getInstanceSettings(inst)
	if err != nil {
		return fmt.Errorf("failed to fetch the settings: %w", err)
	}

	pendingEmail, ok := settings.M["pending_email"].(string)
	if !ok {
		return ErrNoPendingEmail
	}

	err = s.token.Validate(inst, token.EmailUpdate, pendingEmail, tok)
	if err != nil {
		return fmt.Errorf("failed to validate the token: %w", err)
	}

	settings.M["email"] = pendingEmail
	settings.M["pending_email"] = nil

	err = s.storage.setInstanceSettings(inst, settings)
	if err != nil {
		return fmt.Errorf("failed to save the settings changes: %w", err)
	}

	if err := s.instance.SyncEmail(inst, pendingEmail); err != nil {
		return fmt.Errorf("failed to update the instance: %w", err)
	}

	publicName, _ := settings.M["public_name"].(string)
	// if the public name is not defined, use the instance's domain
	if publicName == "" {
		split := strings.Split(inst.DomainName(), ".")
		publicName = split[0]
	}

	err = s.cloudery.SaveInstance(inst, &cloudery.SaveCmd{
		Locale:     inst.Locale,
		Email:      settings.M["email"].(string),
		PublicName: publicName,
	})
	if err != nil {
		return fmt.Errorf("failed to update the cloudery: %w", err)
	}

	return nil
}

// CancelEmailUpdate cancel any ongoing email update process
//
// If no process is ongoin it's a no-op.
func (s *SettingsService) CancelEmailUpdate(inst *instance.Instance) error {
	settings, err := s.storage.getInstanceSettings(inst)
	if err != nil {
		return fmt.Errorf("failed to fetch the settings: %w", err)
	}

	_, ok := settings.M["pending_email"].(string)
	if !ok {
		return nil
	}

	settings.M["pending_email"] = nil

	err = s.storage.setInstanceSettings(inst, settings)
	if err != nil {
		return fmt.Errorf("failed to save the settings changes: %w", err)
	}

	return nil
}

func (s *SettingsService) GetExternalTies(inst *instance.Instance) (*ExternalTies, error) {
	blockingSubscription, err := s.cloudery.BlockingSubscription(inst)
	if err != nil {
		return nil, err
	}

	var ties ExternalTies
	if blockingSubscription != nil {
		ties = ExternalTies{
			HasBlockingSubscription: true,
			BlockingSubscription:    blockingSubscription,
		}
	}

	return &ties, nil
}

func (s *SettingsService) GetLegalNoticeUrl(inst *instance.Instance) (string, error) {
	return s.cloudery.LegalNoticeUrl(inst)
}
