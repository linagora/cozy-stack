// Package publiclink defines the persisted model for public-link activity.
package publiclink

import (
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// Action identifies an action on the link itself, not on an individual item.
type Action string

const (
	Created    Action = "created"
	Updated    Action = "updated"
	Opened     Action = "opened"
	Downloaded Action = "downloaded"
)

// Event is stored in the instance that owns the link. LinkID is an opaque,
// non-secret identifier; it must not contain the public link's sharecode.
// Identity is omitted when no Cozy identity has been reliably established.
type Event struct {
	DocID      string    `json:"_id,omitempty"`
	DocRev     string    `json:"_rev,omitempty"`
	LinkID     string    `json:"link_id"`
	Action     Action    `json:"action"`
	OccurredAt time.Time `json:"occurred_at"`
	IP         string    `json:"ip"`
	Identity   string    `json:"identity,omitempty"`
}

func (e *Event) DocType() string   { return consts.PublicLinkEvents }
func (e *Event) ID() string        { return e.DocID }
func (e *Event) SetID(id string)   { e.DocID = id }
func (e *Event) Rev() string       { return e.DocRev }
func (e *Event) SetRev(rev string) { e.DocRev = rev }
func (e *Event) Clone() couchdb.Doc {
	clone := *e
	return &clone
}
