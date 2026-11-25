// Package search is the server-side file search, backed by OpenSearch.
package search

import (
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// FileDocument is a file or folder in the files_v1 index. It has no path
// and no member list, so moving or sharing a folder never rewrites its files.
type FileDocument struct {
	ResourceKey     string `json:"resource_key"`
	OwnerInstanceID string `json:"owner_instance_id"`
	DocID           string `json:"doc_id"`
	DirID           string `json:"dir_id"`
	ScopedDirKey    string `json:"scoped_dir_key"`
	Type            string `json:"type"`
	Name            string `json:"name"`
	Mime            string `json:"mime,omitempty"`
	Class           string `json:"class,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
	Trashed         bool   `json:"trashed"`
}

// Key returns the key of a document of an instance in the search indices.
func Key(domain, docID string) string {
	return domain + ":" + docID
}

// NewFileDocument maps an io.cozy.files document from the changes feed of the
// instance to its search document. It returns false for the documents that
// are not indexed: deleted docs (the caller deletes their key), design docs,
// docs that are neither a file nor a folder, the root and the trash folders.
func NewFileDocument(domain string, doc couchdb.JSONDoc) (*FileDocument, bool) {
	id, _ := doc.M["_id"].(string)
	if deleted, _ := doc.M["_deleted"].(bool); deleted || id == "" ||
		strings.HasPrefix(id, "_design/") ||
		id == consts.RootDirID || id == consts.TrashDirID {
		return nil, false
	}
	str := func(key string) string { s, _ := doc.M[key].(string); return s }

	fd := &FileDocument{
		ResourceKey:     Key(domain, id),
		OwnerInstanceID: domain,
		DocID:           id,
		DirID:           str("dir_id"),
		Type:            str("type"),
		Name:            str("name"),
		Mime:            str("mime"),
		Class:           str("class"),
		UpdatedAt:       str("updated_at"),
	}
	if fd.Type != consts.FileType && fd.Type != consts.DirType {
		return nil, false
	}
	if fd.Type == consts.DirType {
		// A folder is in its own scope, so the root of a shared drive is found
		fd.ScopedDirKey = fd.ResourceKey
		fd.Trashed = vfs.IsInTrash(str("path"))
	} else {
		fd.ScopedDirKey = Key(domain, fd.DirID)
		// Trashing a folder also marks the files inside as trashed
		// (setTrashedForFilesInsideDir), except for a folder replicated by a
		// sharing, which is only moved. VisiblePaths hides those, and the
		// files not indexed again yet.
		fd.Trashed, _ = doc.M["trashed"].(bool)
	}
	return fd, true
}
