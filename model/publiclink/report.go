package publiclink

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// ReportMessage contains only non-secret identifiers, never activity or tokens.
type ReportMessage struct {
	LinkID string    `json:"link_id"`
	Expiry time.Time `json:"expiry"`
}

// Report is an ordinary private document so checkpoints survive instance moves.
type Report struct {
	DocID  string `json:"_id,omitempty"`
	DocRev string `json:"_rev,omitempty"`
	ReportMessage
	FileID      string    `json:"file_id"`
	FileRev     string    `json:"file_rev,omitempty"`
	DirectoryID string    `json:"directory_id,omitempty"`
	Status      string    `json:"status"`
	NoEmail     bool      `json:"no_email,omitempty"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt,omitempty"`
}

func ReportID(linkID string, expiry time.Time) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(linkID+"/"+expiry.UTC().Format(time.RFC3339Nano))))
}

func (r *Report) DocType() string    { return consts.PublicLinkReports }
func (r *Report) ID() string         { return r.DocID }
func (r *Report) Rev() string        { return r.DocRev }
func (r *Report) SetID(id string)    { r.DocID = id }
func (r *Report) SetRev(rev string)  { r.DocRev = rev }
func (r *Report) Clone() couchdb.Doc { clone := *r; return &clone }
func (r *Report) Terminal() bool {
	return r.Status == "sent" || r.Status == "no-email" || r.Status == "cancelled" || r.Status == "failed"
}
