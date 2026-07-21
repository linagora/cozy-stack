package orgdirectory

import (
	"fmt"
	"strings"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// ContactPatch describes a B2B contact replica to create or update.
type ContactPatch struct {
	OrganizationID string
	Username       string
	Email          string
	FirstName      string
	LastName       string
	WorkplaceFQDN  string
	Name           string
	CozyURL        string
	Phone          string
}

func (patch ContactPatch) validate() error {
	if strings.TrimSpace(patch.Email) == "" {
		return fmt.Errorf("contact missing email")
	}
	return nil
}

func (patch ContactPatch) shouldSkipOwn(inst *instance.Instance) bool {
	if patch.WorkplaceFQDN != "" && inst.HasDomain(utils.ExtractInstanceHost(patch.WorkplaceFQDN)) {
		return true
	}
	if patch.Email == "" {
		return false
	}
	email, err := inst.SettingsEMail()
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(email), patch.Email)
}

func (patch ContactPatch) displayName() string {
	if patch.Name != "" {
		return patch.Name
	}
	name := strings.TrimSpace(patch.FirstName + " " + patch.LastName)
	if name != "" {
		return name
	}
	if patch.Username != "" {
		return patch.Username
	}
	if patch.Email != "" {
		parts := strings.SplitN(patch.Email, "@", 2)
		return parts[0]
	}
	return ""
}

func contactPatchFromMember(organizationID string, member GroupMember) ContactPatch {
	workplaceFQDN := strings.TrimSpace(member.WorkplaceFQDN)
	return ContactPatch{
		OrganizationID: strings.TrimSpace(organizationID),
		Username:       strings.TrimSpace(member.Username),
		Email:          strings.TrimSpace(member.Email),
		FirstName:      strings.TrimSpace(member.FirstName),
		LastName:       strings.TrimSpace(member.LastName),
		WorkplaceFQDN:  workplaceFQDN,
		CozyURL:        cozyURLFromWorkplaceFQDN(workplaceFQDN),
	}
}

func contactPatchFromContact(organizationID string, contactDoc *contact.Contact) ContactPatch {
	meta := directoryMetadata(&contactDoc.JSONDoc)
	patch := ContactPatch{OrganizationID: organizationID}
	patch.Username = meta.Username
	patch.Email = meta.Email
	patch.WorkplaceFQDN = meta.WorkplaceFQDN
	patch.CozyURL = contactDoc.PrimaryCozyURL()
	if patch.CozyURL == "" {
		patch.CozyURL = cozyURLFromWorkplaceFQDN(patch.WorkplaceFQDN)
	}
	patch.Name = primaryContactName(contactDoc)
	patch.Phone = contactDoc.PrimaryPhoneNumber()
	return patch
}

func primaryContactName(contactDoc *contact.Contact) string {
	name := contactDoc.PrimaryName()
	if name != "" {
		return name
	}
	displayName, _ := contactDoc.M["displayName"].(string)
	return displayName
}

func cozyURLFromWorkplaceFQDN(workplaceFQDN string) string {
	domain := utils.ExtractInstanceHost(workplaceFQDN)
	if domain == "" {
		return ""
	}
	return "https://" + domain
}
