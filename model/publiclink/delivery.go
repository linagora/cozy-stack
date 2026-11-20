package publiclink

import (
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// ReportWorker is reserved for the stack's expired link scheduler.
const ReportWorker = "expired-link-report-mail"

const (
	DeliveryPending    = "pending"
	DeliveryFileOnly   = "file-only"
	DeliveryNoEmail    = "no-email"
	DeliverySubmitting = "submitting"
	DeliverySent       = "sent"
)

// DeliveryID is distinct from the report file attestation and from the prototype's checkpoint.
func DeliveryID(reportID string) string { return "delivery-" + reportID }

// Delivery tracks delivery separately from the immutable file attestation.
type Delivery struct {
	DocID       string    `json:"_id,omitempty"`
	DocRev      string    `json:"_rev,omitempty"`
	Status      string    `json:"status"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
}

func (d *Delivery) ID() string         { return d.DocID }
func (d *Delivery) Rev() string        { return d.DocRev }
func (d *Delivery) SetID(id string)    { d.DocID = id }
func (d *Delivery) SetRev(rev string)  { d.DocRev = rev }
func (d *Delivery) DocType() string    { return consts.PublicLinkReports }
func (d *Delivery) Clone() couchdb.Doc { clone := *d; return &clone }

// Due excludes terminal and unknown states, including an ambiguous SMTP submission.
func (d *Delivery) Due(now time.Time) bool {
	return (d.Status == DeliveryPending || d.Status == DeliveryFileOnly) && !d.NextAttempt.After(now)
}
