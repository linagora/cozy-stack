package publiclink

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// ReportMessage identifies an expired link report within the job's instance.
type ReportMessage struct {
	LinkID string    `json:"link_id"`
	Expiry time.Time `json:"expiry"`
}

// ReportID identifies the report for a link and one exact expiry instant.
func ReportID(linkID string, expiry time.Time) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(linkID+"/"+expiry.UTC().Format(time.RFC3339Nano))))
}
