package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// SyncCreatedOrgContact syncs a newly created B2B user as an external contact
// into every other instance of the organization.
func SyncCreatedOrgContact(ctx context.Context, target *instance.Instance, msg UserCreatedMessage) error {
	scope, err := orgdirectory.ResolveOrganizationInstances(msg.OrganizationID, msg.OrganizationDomain)
	if err != nil {
		return fmt.Errorf("user.created: %w", err)
	}
	return syncCreatedOrgContact(ctx, target, msg, scope)
}

func syncCreatedOrgContact(ctx context.Context, target *instance.Instance, msg UserCreatedMessage, scope orgdirectory.OrganizationInstances) error {
	email := strings.TrimSpace(msg.InternalEmail)
	if email == "" {
		return fmt.Errorf("user.created: missing internalEmail for organization contact sync")
	}
	if scope.OrganizationID == "" {
		return fmt.Errorf("user.created: missing organizationId")
	}

	name, err := target.SettingsPublicName()
	if err != nil {
		return fmt.Errorf("user.created: resolve contact name for %s: %w", target.Domain, err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("user.created: missing public_name in settings for %s", target.Domain)
	}
	targetURL := target.PageURL("", nil)
	workplaceFQDN := strings.TrimSpace(msg.WorkplaceFqdn)

	var lastErr error
	for _, inst := range scope.Instances {
		if err := ctx.Err(); err != nil {
			return err
		}
		if inst.HasDomain(workplaceFQDN) {
			log.Debugf("user.created: creating contacts for own instance %s for organization contact %s", inst.Domain, targetURL)
			if err := syncExistingOrgContactsToCreatedUser(ctx, target, scope.Instances, workplaceFQDN, scope.OrganizationID); err != nil {
				log.Errorf("%v", err)
				lastErr = err
			}
			continue
		}

		if _, err := orgdirectory.UpsertManagedContact(inst, orgdirectory.ContactPatch{
			OrganizationID: scope.OrganizationID,
			Email:          email,
			Name:           name,
			CozyURL:        targetURL,
			Phone:          strings.TrimSpace(msg.Mobile),
			WorkplaceFQDN:  workplaceFQDN,
		}); err != nil {
			wrappedErr := fmt.Errorf("user.created: sync contact for %s in %s: %w", email, inst.Domain, err)
			log.Errorf("%v", wrappedErr)
			lastErr = wrappedErr
			continue
		}
		log.Infof("user.created: synced organization contact for %s in %s", email, inst.Domain)
	}

	return lastErr
}

// SyncDeletedOrgContact removes a deleted B2B user from every other instance
// of the organization.
func SyncDeletedOrgContact(ctx context.Context, msg UserDeletedMessage) error {
	if strings.TrimSpace(msg.WorkplaceFqdn) == "" {
		return fmt.Errorf("user.deleted: missing workplaceFqdn")
	}
	email := strings.TrimSpace(msg.InternalEmail)
	if email == "" {
		return fmt.Errorf("user.deleted: missing internalEmail")
	}

	scope, err := orgdirectory.ResolveOrganizationInstances(msg.OrganizationID, msg.Domain)
	if err != nil {
		return fmt.Errorf("user.deleted: %w", err)
	}

	var lastErr error
	for _, inst := range scope.Instances {
		if err := ctx.Err(); err != nil {
			return err
		}
		if inst.HasDomain(msg.WorkplaceFqdn) {
			log.Debugf("user.deleted: skipping own instance %s for organization contact %s", inst.Domain, msg.WorkplaceFqdn)
			continue
		}

		existing, err := findExternalOrgContactByEmail(inst, email)
		if err != nil {
			wrappedErr := fmt.Errorf("user.deleted: find external contact for %s in %s: %w", email, inst.Domain, err)
			log.Errorf("%v", wrappedErr)
			lastErr = wrappedErr
			continue
		}
		if existing == nil {
			log.Infof("user.deleted: no external contact for %s in %s, skipping", email, inst.Domain)
			continue
		}
		if err := couchdb.DeleteDoc(inst, existing); err != nil {
			wrappedErr := fmt.Errorf("user.deleted: delete contact %s for %s in %s: %w", existing.ID(), email, inst.Domain, err)
			log.Errorf("%v", wrappedErr)
			lastErr = wrappedErr
			continue
		}
		log.Infof("user.deleted: deleted organization contact for %s in %s", email, inst.Domain)
	}

	return lastErr
}

func syncExistingOrgContactsToCreatedUser(ctx context.Context, target *instance.Instance, instances []*instance.Instance, workplaceFqdn, organizationID string) error {
	var lastErr error
	for _, inst := range instances {
		if err := ctx.Err(); err != nil {
			return err
		}
		if inst.HasDomain(workplaceFqdn) {
			continue
		}

		input, err := externalOrgContactFromInstance(inst, organizationID)
		if err != nil {
			wrappedErr := fmt.Errorf("user.created: build existing user contact for %s: %w", inst.Domain, err)
			log.Errorf("%v", wrappedErr)
			lastErr = wrappedErr
			continue
		}

		if _, err := orgdirectory.UpsertManagedContact(target, input); err != nil {
			wrappedErr := fmt.Errorf("user.created: sync contact for %s in %s: %w", input.Email, target.Domain, err)
			log.Errorf("%v", wrappedErr)
			lastErr = wrappedErr
			continue
		}
		log.Infof("user.created: synced organization contact for %s in %s", input.Email, target.Domain)
	}
	return lastErr
}

func externalOrgContactFromInstance(inst *instance.Instance, organizationID string) (orgdirectory.ContactPatch, error) {
	settings, err := inst.SettingsDocument()
	if err != nil {
		return orgdirectory.ContactPatch{}, err
	}

	email, _ := settings.M["email"].(string)
	email = strings.TrimSpace(email)
	if email == "" {
		return orgdirectory.ContactPatch{}, fmt.Errorf("missing email in settings")
	}

	name, _ := settings.M["public_name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return orgdirectory.ContactPatch{}, fmt.Errorf("missing public_name in settings")
	}

	phone, _ := settings.M["phone"].(string)

	return orgdirectory.ContactPatch{
		OrganizationID: organizationID,
		Email:          email,
		Name:           name,
		CozyURL:        inst.PageURL("", nil),
		Phone:          strings.TrimSpace(phone),
		WorkplaceFQDN:  strings.TrimSpace(inst.Domain),
	}, nil
}

func findExternalOrgContactByEmail(inst *instance.Instance, email string) (*contact.Contact, error) {
	matches, err := contact.FindAllByEmail(inst, email)
	if errors.Is(err, contact.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var external []*contact.Contact
	for _, doc := range matches {
		if doc.IsExternal() {
			external = append(external, doc)
		}
	}
	if len(external) > 1 {
		return nil, fmt.Errorf("multiple external contacts found for email %s", email)
	}
	if len(external) == 1 {
		return external[0], nil
	}
	return nil, nil
}
