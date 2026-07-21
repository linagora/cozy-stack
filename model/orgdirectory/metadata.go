package orgdirectory

import (
	"github.com/go-viper/mapstructure/v2"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

const (
	DirectoryMetadataKey = "twakeDirectory"
	metadataKindGroup    = "group"
	metadataKindContact  = "contact"
)

// DirectoryMetadata describes the B2B organization directory ownership stored
// on managed contact and group documents.
type DirectoryMetadata struct {
	Managed        bool   `json:"managed" mapstructure:"managed"`
	Kind           string `json:"kind,omitempty" mapstructure:"kind"`
	OrganizationID string `json:"organizationId,omitempty" mapstructure:"organizationId"`
	ExternalID     string `json:"externalId,omitempty" mapstructure:"externalId"`
	Username       string `json:"username,omitempty" mapstructure:"username"`
	Email          string `json:"email,omitempty" mapstructure:"email"`
	WorkplaceFQDN  string `json:"workplaceFqdn,omitempty" mapstructure:"workplaceFqdn"`
}

// IsManagedDirectoryDoctype reports whether a doctype can contain managed
// organization directory documents.
func IsManagedDirectoryDoctype(doctype string) bool {
	return doctype == consts.Contacts || doctype == consts.Groups
}

// IsManagedDirectoryDoc reports whether a contact or group document is managed
// by the B2B organization directory replication.
func IsManagedDirectoryDoc(doc *couchdb.JSONDoc) bool {
	meta := directoryMetadata(doc)
	return meta.Managed
}

func directoryMetadata(doc *couchdb.JSONDoc) DirectoryMetadata {
	if doc == nil || doc.M == nil {
		return DirectoryMetadata{}
	}
	return decodeDirectoryMetadata(doc.M[DirectoryMetadataKey])
}

func decodeDirectoryMetadata(raw interface{}) DirectoryMetadata {
	if raw == nil {
		return DirectoryMetadata{}
	}
	var meta DirectoryMetadata
	if err := mapstructure.Decode(raw, &meta); err != nil {
		return DirectoryMetadata{}
	}
	return meta
}

func setGroupDirectoryMetadata(doc *couchdb.JSONDoc, organizationID, externalID string) {
	if doc.M == nil {
		doc.M = make(map[string]interface{})
	}
	doc.M[DirectoryMetadataKey] = DirectoryMetadata{
		Managed:        true,
		Kind:           metadataKindGroup,
		OrganizationID: organizationID,
		ExternalID:     externalID,
	}
}

func setContactDirectoryMetadata(doc *couchdb.JSONDoc, input ContactPatch, email string) {
	if doc.M == nil {
		doc.M = make(map[string]interface{})
	}
	doc.M[DirectoryMetadataKey] = DirectoryMetadata{
		Managed:        true,
		Kind:           metadataKindContact,
		OrganizationID: input.OrganizationID,
		Username:       input.Username,
		Email:          email,
		WorkplaceFQDN:  input.WorkplaceFQDN,
	}
}
