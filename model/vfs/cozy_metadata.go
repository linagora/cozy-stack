package vfs

import (
	"time"

	"github.com/cozy/cozy-stack/pkg/metadata"
)

// DocTypeVersion represents the doctype version. Each time this document
// structure is modified, update this value
const DocTypeVersion = "1"

// UploadedByEntry is a struct with information on the app that has done the
// last upload of a file.
type UploadedByEntry struct {
	Slug    string            `json:"slug,omitempty"`
	Version string            `json:"version,omitempty"`
	Client  map[string]string `json:"oauthClient,omitempty"`
}

const (
	// TrashedByKindMember identifies a concrete authenticated/member actor.
	TrashedByKindMember = "member"
	// TrashedByKindAnonymousShare identifies anonymous/public share access.
	TrashedByKindAnonymousShare = "anonymous-share"
)

// TrashedByEntry identifies who sent a file or folder to the trash.
type TrashedByEntry struct {
	Kind        string `json:"kind,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Domain      string `json:"domain,omitempty"`
}

// RAGMetadata holds the RAG indexation status for a file.
type RAGMetadata struct {
	Indexed         bool       `json:"indexed"`
	Status          string     `json:"status,omitempty"`
	LastSuccessDate *time.Time `json:"lastSuccessDate,omitempty"`
	LastErrorDate   *time.Time `json:"lastErrorDate,omitempty"`
}

// FilesCozyMetadata is an extended version of cozyMetadata with some specific fields.
type FilesCozyMetadata struct {
	metadata.CozyMetadata
	// Instance URL where the file has been created
	CreatedOn string `json:"createdOn,omitempty"`
	// Date of the last upload of a new content
	UploadedAt *time.Time `json:"uploadedAt,omitempty"`
	// Information about the last time the content was uploaded
	UploadedBy *UploadedByEntry `json:"uploadedBy,omitempty"`
	// Instance URL where the content has been changed the last time
	UploadedOn string `json:"uploadedOn,omitempty"`
	// Date of the last trash action
	TrashedAt *time.Time `json:"trashedAt,omitempty"`
	// Information about who sent the file or folder to the trash
	TrashedBy *TrashedByEntry `json:"trashedBy,omitempty"`
	// RAG indexation status
	RAG *RAGMetadata `json:"rag,omitempty"`
}

// NewCozyMetadata initializes a new FilesCozyMetadata struct
func NewCozyMetadata(instance string) *FilesCozyMetadata {
	now := time.Now()
	return &FilesCozyMetadata{
		CozyMetadata: metadata.CozyMetadata{
			DocTypeVersion:  DocTypeVersion,
			MetadataVersion: metadata.MetadataVersion,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		CreatedOn: instance,
	}
}

// Clone clones a FileCozyMetadata struct
func (fcm *FilesCozyMetadata) Clone() *FilesCozyMetadata {
	cloned := *fcm
	cloned.UpdatedByApps = make([]*metadata.UpdatedByAppEntry, len(fcm.UpdatedByApps))
	copy(cloned.UpdatedByApps, fcm.UpdatedByApps)
	if fcm.UploadedBy != nil {
		client := make(map[string]string)
		for k, v := range fcm.UploadedBy.Client {
			client[k] = v
		}
		cloned.UploadedBy = &UploadedByEntry{
			Slug:    fcm.UploadedBy.Slug,
			Version: fcm.UploadedBy.Version,
			Client:  client,
		}
	}
	if fcm.UploadedAt != nil {
		at := *fcm.UploadedAt
		cloned.UploadedAt = &at
	}
	if fcm.TrashedAt != nil {
		at := *fcm.TrashedAt
		cloned.TrashedAt = &at
	}
	if fcm.TrashedBy != nil {
		cloned.TrashedBy = &TrashedByEntry{
			Kind:        fcm.TrashedBy.Kind,
			DisplayName: fcm.TrashedBy.DisplayName,
			Domain:      fcm.TrashedBy.Domain,
		}
	}
	if fcm.RAG != nil {
		rag := *fcm.RAG
		if fcm.RAG.LastSuccessDate != nil {
			at := *fcm.RAG.LastSuccessDate
			rag.LastSuccessDate = &at
		}
		if fcm.RAG.LastErrorDate != nil {
			at := *fcm.RAG.LastErrorDate
			rag.LastErrorDate = &at
		}
		cloned.RAG = &rag
	}
	return &cloned
}

// UpdatedByApp updates the list of UpdatedByApps entries with the new entry.
// It ensures that each entry has a unique slug+instance, and the new entry
// will be in the last position.
func (fcm *FilesCozyMetadata) UpdatedByApp(entry *metadata.UpdatedByAppEntry) {
	if entry.Slug == "" {
		return
	}

	i := 0
	seen := make(map[string]struct{})
	for _, app := range fcm.UpdatedByApps {
		if app.Slug == entry.Slug && app.Instance == entry.Instance {
			continue
		}
		key := app.Slug + "/" + app.Instance
		if _, ok := seen[key]; ok {
			continue
		}
		fcm.UpdatedByApps[i] = app
		seen[key] = struct{}{}
		i++
	}

	fcm.UpdatedByApps = append(fcm.UpdatedByApps[:i], entry)
}

// ToJSONDoc returns a map with the cozyMetadata to be used inside a JSONDoc
// (typically for sendings sharing updates).
func (fcm *FilesCozyMetadata) ToJSONDoc() map[string]interface{} {
	doc := make(map[string]interface{})
	doc["doctypeVersion"] = fcm.DocTypeVersion
	doc["metadataVersion"] = fcm.MetadataVersion
	doc["createdAt"] = fcm.CreatedAt
	if fcm.CreatedByApp != "" {
		doc["createdByApp"] = fcm.CreatedByApp
	}
	if fcm.CreatedByAppVersion != "" {
		doc["createdByAppVersion"] = fcm.CreatedByAppVersion
	}
	if fcm.CreatedOn != "" {
		doc["createdOn"] = fcm.CreatedOn
	}

	doc["updatedAt"] = fcm.UpdatedAt
	if len(fcm.UpdatedByApps) > 0 {
		entries := make([]map[string]interface{}, len(fcm.UpdatedByApps))
		for i, entry := range fcm.UpdatedByApps {
			subdoc := map[string]interface{}{
				"slug": entry.Slug,
				"date": entry.Date,
			}
			if entry.Version != "" {
				subdoc["version"] = entry.Version
			}
			if entry.Instance != "" {
				subdoc["instance"] = entry.Instance
			}
			entries[i] = subdoc
		}
		doc["uploadedByApp"] = entries
	}

	if fcm.UploadedAt != nil {
		doc["uploadedAt"] = *fcm.UploadedAt
	}
	if fcm.UploadedBy != nil {
		uploaded := make(map[string]interface{})
		if fcm.UploadedBy.Slug != "" {
			uploaded["slug"] = fcm.UploadedBy.Slug
		}
		if fcm.UploadedBy.Version != "" {
			uploaded["slug"] = fcm.UploadedBy.Version
		}
		if len(fcm.UploadedBy.Client) > 0 {
			uploaded["oauthClient"] = fcm.UploadedBy.Client
		}
		doc["uploadedBy"] = uploaded
	}
	if fcm.UploadedOn != "" {
		doc["uploadedOn"] = fcm.UploadedOn
	}
	if fcm.TrashedAt != nil {
		doc["trashedAt"] = *fcm.TrashedAt
	}
	if fcm.TrashedBy != nil {
		trashed := make(map[string]interface{})
		if fcm.TrashedBy.Kind != "" {
			trashed["kind"] = fcm.TrashedBy.Kind
		}
		if fcm.TrashedBy.DisplayName != "" {
			trashed["displayName"] = fcm.TrashedBy.DisplayName
		}
		if fcm.TrashedBy.Domain != "" {
			trashed["domain"] = fcm.TrashedBy.Domain
		}
		doc["trashedBy"] = trashed
	}
	if fcm.RAG != nil {
		// Serialize via the struct's json tags so every field is handled
		// uniformly (indexed, status, and the dates).
		doc["rag"] = fcm.RAG
	}
	if fcm.SourceAccount != "" {
		doc["sourceAccount"] = fcm.SourceAccount
	}
	if fcm.SourceIdentifier != "" {
		doc["sourceAccountIdentifier"] = fcm.SourceIdentifier
	}
	return doc
}
