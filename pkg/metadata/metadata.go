// Package metadata is used for manipulating the cozyMetadata field of the
// documents.
package metadata

import (
	"errors"
	"time"
)

// MetadataVersion represents the CozyMetadata version used.
const MetadataVersion = 1

// ErrSlugEmpty is returned when an UpdatedByApp entry is created with and empty
// slug
var ErrSlugEmpty = errors.New("Slug cannot be empty")

// UpdatedByAppEntry represents a modification made by an application to the
// document
type UpdatedByAppEntry struct {
	Slug     string    `json:"slug"`               // Slug of the app
	Date     time.Time `json:"date"`               // Date of the update
	Version  string    `json:"version,omitempty"`  // Version identifier of the app
	Instance string    `json:"instance,omitempty"` // URL of the instance
}

// CozyMetadata holds all the metadata of a document
type CozyMetadata struct {
	// Name or identifier for the version of the schema used by this document
	DocTypeVersion string `json:"doctypeVersion"`
	// Version of the cozyMetadata
	MetadataVersion int `json:"metadataVersion"`
	// Tell if the document if part of the favorites
	Favorite bool `json:"favorite,omitempty"`
	// Creation date of the cozy document
	CreatedAt time.Time `json:"createdAt"`
	// Slug of the app or konnector which created the document
	CreatedByApp string `json:"createdByApp,omitempty"`
	// Version identifier of the app
	CreatedByAppVersion string `json:"createdByAppVersion,omitempty"`
	// Last modification date of the cozy document
	UpdatedAt time.Time `json:"updatedAt"`
	// List of objects representing the applications which modified the cozy document
	UpdatedByApps []*UpdatedByAppEntry `json:"updatedByApps,omitempty"`
	// Identifier of the account in io.cozy.accounts (for konnectors)
	SourceAccount string `json:"sourceAccount,omitempty"`
	// Identifier unique to the account targeted by the connector (login most of the time)
	SourceIdentifier string `json:"sourceAccountIdentifier,omitempty"`
}

// New initializes a new CozyMetadata structure
func New() *CozyMetadata {
	now := time.Now()
	return &CozyMetadata{
		MetadataVersion: MetadataVersion,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// NewWithApp initializes a CozyMetadata with a slug and a version
// Version is optional
func NewWithApp(slug, version, doctypeVersion string) (*CozyMetadata, error) {
	if slug == "" {
		return nil, ErrSlugEmpty
	}
	md := New()
	md.CreatedByApp = slug
	if version != "" {
		md.CreatedByAppVersion = version
	}
	md.DocTypeVersion = doctypeVersion

	err := md.UpdatedByApp(slug, version)
	if err != nil {
		return nil, err
	}
	return md, nil
}

// Clone clones a CozyMetadata struct
func (cm *CozyMetadata) Clone() *CozyMetadata {
	cloned := *cm
	cloned.UpdatedByApps = make([]*UpdatedByAppEntry, len(cm.UpdatedByApps))
	copy(cloned.UpdatedByApps, cm.UpdatedByApps)
	return &cloned
}

// EnsureCreatedFields ensures that empty fields are filled, otherwise use
// the default metadata values during the creation process
func (cm *CozyMetadata) EnsureCreatedFields(defaultMetadata *CozyMetadata) {
	if cm.UpdatedAt.IsZero() {
		cm.UpdatedAt = defaultMetadata.UpdatedAt
	}
	if cm.CreatedByApp == "" {
		cm.CreatedByApp = defaultMetadata.CreatedByApp
	}
	if cm.DocTypeVersion == "" {
		cm.DocTypeVersion = defaultMetadata.DocTypeVersion
	}
	if cm.MetadataVersion == 0 {
		cm.MetadataVersion = defaultMetadata.MetadataVersion
	}
	if cm.UpdatedByApps == nil {
		cm.UpdatedByApps = defaultMetadata.UpdatedByApps
	}
}

// ChangeUpdatedAt updates the UpdatedAt timestamp
func (cm *CozyMetadata) ChangeUpdatedAt() {
	cm.UpdatedAt = time.Now()
}

// UpdatedByApp updates an entry either by updating the struct if the
// slug/version already exists or by appending a new entry to the list
func (cm *CozyMetadata) UpdatedByApp(slug, version string) error {
	if slug == "" {
		return ErrSlugEmpty
	}

	now := time.Now()
	cm.UpdatedAt = now
	updated := &UpdatedByAppEntry{Slug: slug, Date: now, Version: version}
	for i, entry := range cm.UpdatedByApps {
		if entry.Slug == slug {
			updated.Instance = entry.Instance
			cm.UpdatedByApps[i] = updated
			return nil
		}
	}

	// The entry has not been found, adding it
	cm.UpdatedByApps = append(cm.UpdatedByApps, updated)
	return nil
}
