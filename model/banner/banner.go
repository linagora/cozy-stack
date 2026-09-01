// Package banner materializes the platform messages displayed to the user as
// io.cozy.banners documents.
//
// Evaluation happens when an input changes, not when a client reads: the rules
// run here and the result is written to the instance database, so reading a
// banner is a plain document fetch with no computation behind it.
package banner

import (
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metadata"
)

// DocTypeVersion is the version of the io.cozy.banners shape written here.
const DocTypeVersion = "1"

// The categories a banner can belong to.
const (
	CategoryQuota   = "quota"
	CategoryBilling = "billing"
	CategoryTrial   = "trial"
	CategoryAccount = "account"
	CategorySystem  = "system"
)

// The severities a client maps to its own alert styles.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// The surfaces a banner can be displayed on.
const (
	SurfaceBanner = "banner"
	SurfaceModal  = "modal"
)

// CTA is the optional call to action of a banner.
type CTA struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Source records what produced a document, so a displayed banner can be
// explained after the fact without replaying the evaluation.
type Source struct {
	Trigger string    `json:"trigger"`
	At      time.Time `json:"at"`
}

// Banner is an io.cozy.banners document.
type Banner struct {
	DocID  string `json:"_id,omitempty"`
	DocRev string `json:"_rev,omitempty"`

	// BannerID identifies the occurrence of the condition. It is deliberately
	// not called id: a root-level id would shadow the attribute cozy-client
	// derives from _id.
	BannerID string `json:"bannerId"`
	Category string `json:"category"`
	Severity string `json:"severity"`
	Surface  string `json:"surface"`
	// Title is the heading of a dialog. A client that finds it missing names
	// the dialog from its text instead.
	Title        string     `json:"title,omitempty"`
	Text         string     `json:"text"`
	Lang         string     `json:"lang"`
	CTA          *CTA       `json:"cta,omitempty"`
	SecondaryCTA *CTA       `json:"secondaryCta,omitempty"`
	Dismissible  bool       `json:"dismissible"`
	DismissedAt  *time.Time `json:"dismissedAt"`
	Priority     int        `json:"priority"`
	StartsAt     *time.Time `json:"startsAt,omitempty"`
	EndsAt       *time.Time `json:"endsAt,omitempty"`
	Source       Source     `json:"source"`

	Metadata *metadata.CozyMetadata `json:"cozyMetadata,omitempty"`
}

func (b *Banner) ID() string         { return b.DocID }
func (b *Banner) Rev() string        { return b.DocRev }
func (b *Banner) DocType() string    { return consts.Banners }
func (b *Banner) SetID(id string)    { b.DocID = id }
func (b *Banner) SetRev(rev string)  { b.DocRev = rev }
func (b *Banner) Clone() couchdb.Doc { return b.clone() }

// clone is Clone, typed, for the callers inside the package.
func (b *Banner) clone() *Banner {
	cloned := *b
	if b.CTA != nil {
		cta := *b.CTA
		cloned.CTA = &cta
	}
	if b.SecondaryCTA != nil {
		cta := *b.SecondaryCTA
		cloned.SecondaryCTA = &cta
	}
	if b.DismissedAt != nil {
		at := *b.DismissedAt
		cloned.DismissedAt = &at
	}
	if b.StartsAt != nil {
		at := *b.StartsAt
		cloned.StartsAt = &at
	}
	if b.EndsAt != nil {
		at := *b.EndsAt
		cloned.EndsAt = &at
	}
	if b.Metadata != nil {
		cloned.Metadata = b.Metadata.Clone()
	}
	return &cloned
}

var _ couchdb.Doc = &Banner{}
