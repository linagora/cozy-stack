package rag

import (
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
)

// IndexStatus is the RAG indexation status of a document. Its identifier is
// the identifier of the document it describes.
type IndexStatus struct {
	CouchID  string `json:"_id,omitempty"`
	CouchRev string `json:"_rev,omitempty"`

	Indexed bool   `json:"indexed"`
	Status  string `json:"status,omitempty"`
	// Revision of the io.cozy.files document this status describes. Callbacks
	// are ordered on it.
	DocRev          string     `json:"docRev,omitempty"`
	LastSuccessDate *time.Time `json:"lastSuccessDate,omitempty"`
	LastErrorDate   *time.Time `json:"lastErrorDate,omitempty"`

	// Read it with relData["_id"], not with Relationship.ResourceIdentifier():
	// the latter looks for "id"/"type" and would silently return an empty
	// reference.
	Rels jsonapi.RelationshipMap `json:"relationships,omitempty"`
}

func (s *IndexStatus) ID() string        { return s.CouchID }
func (s *IndexStatus) Rev() string       { return s.CouchRev }
func (s *IndexStatus) DocType() string   { return consts.ChatRAG }
func (s *IndexStatus) SetID(id string)   { s.CouchID = id }
func (s *IndexStatus) SetRev(rev string) { s.CouchRev = rev }

func (s *IndexStatus) Clone() couchdb.Doc {
	cloned := *s
	if s.LastSuccessDate != nil {
		at := *s.LastSuccessDate
		cloned.LastSuccessDate = &at
	}
	if s.LastErrorDate != nil {
		at := *s.LastErrorDate
		cloned.LastErrorDate = &at
	}
	cloned.Rels = s.Rels.Clone()
	return &cloned
}

func NewIndexStatus(docID string) *IndexStatus {
	return &IndexStatus{
		CouchID: docID,
		Rels: jsonapi.RelationshipMap{
			"doc": jsonapi.Relationship{
				Data: struct {
					ID   string `json:"_id"`
					Type string `json:"_type"`
				}{ID: docID, Type: consts.Files},
			},
		},
	}
}
