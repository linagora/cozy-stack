package banner

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// TriggerCommand is recorded on documents a backend asked for rather than a
// rule the stack runs itself.
const TriggerCommand = "banner.command"

// ErrInvalidCommand marks a command no retry can fix. A transport rejects such
// a delivery instead of requeuing it; anything else is a storage failure worth
// retrying.
var ErrInvalidCommand = errors.New("invalid banner command")

// Localized is wording keyed by locale, as the backend sends it.
type Localized map[string]string

// CommandCTA is a call to action before a locale has been picked for it.
type CommandCTA struct {
	Label Localized `json:"label"`
	URL   string    `json:"url"`
}

// Command is what a backend asks for, independent of how it arrived.
type Command struct {
	Category string `json:"category"`

	// Exactly one of Domain and WorkplaceFqdn is set. Domain addresses a
	// B2B organization, and every instance under it gets the banner.
	Domain        string `json:"domain,omitempty"`
	WorkplaceFqdn string `json:"workplaceFqdn,omitempty"`

	// EventID is the backend's correlation id, logged for traceability.
	EventID string `json:"eventId,omitempty"`
	// Revision is a positive counter the backend increments per target and
	// category. Delivery is at-least-once and unordered, so this (not the
	// arrival time) orders a command against what is stored.
	Revision int64 `json:"revision"`
	// Timestamp is when the backend decided, in epoch seconds. Provenance
	// only: it orders nothing.
	Timestamp int64 `json:"timestamp"`

	// Clear empties the category instead of materializing into it. Set by the
	// transport (routing key), never read from the payload.
	Clear bool `json:"-"`

	BannerID     string      `json:"bannerId,omitempty"`
	Severity     string      `json:"severity,omitempty"`
	Surface      string      `json:"surface,omitempty"`
	Title        Localized   `json:"title,omitempty"`
	Text         Localized   `json:"text,omitempty"`
	CTA          *CommandCTA `json:"cta,omitempty"`
	SecondaryCTA *CommandCTA `json:"secondaryCta,omitempty"`
	Dismissible  bool        `json:"dismissible,omitempty"`
	Priority     int         `json:"priority,omitempty"`
	StartsAt     *time.Time  `json:"startsAt,omitempty"`
	EndsAt       *time.Time  `json:"endsAt,omitempty"`
}

// The boundary rejects rather than repairs: anything unexpected is a backend bug.
var (
	categoryFormat = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	bannerIDFormat = regexp.MustCompile(`^[a-z0-9.-]{1,64}$`)
	targetFormat   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,253}[A-Za-z0-9])?$`)
)

const (
	maxLabelLen   = 128
	maxTitleLen   = 256
	maxTextLen    = 1024
	maxURLLen     = 2048
	maxPriority   = 1000
	maxEventIDLen = 256
	maxLocaleLen  = 35
	// MaxCommandBytes bounds the JSON body at the transport boundary.
	MaxCommandBytes = 256 * 1024
	// maxLocales bounds the locale map, not each value.
	maxLocales = 32
)

// ApplyCommand materializes or clears the banner a backend asked for.
func ApplyCommand(cmd Command) error {
	if err := cmd.validate(); err != nil {
		return err
	}

	instances, err := cmd.targets()
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if inst.HasBannersEnabled() && !inst.AllowsBannerCategory(cmd.Category) {
			return fmt.Errorf("%w: the context of %s does not accept commands for the %s category",
				ErrInvalidCommand, inst.Domain, cmd.Category)
		}
	}
	for _, inst := range instances {
		if err := cmd.applyTo(inst); err != nil {
			return fmt.Errorf("%s: %w", inst.Domain, err)
		}
	}
	return nil
}

// targets resolves what the backend addressed. An organization with no
// instance is a no-op. A missing workplace is retryable (not invalid): the
// stack cannot tell a deleted instance from one still being provisioned.
func (cmd Command) targets() ([]*instance.Instance, error) {
	if cmd.Domain != "" {
		list, err := lifecycle.ListOrgInstances(cmd.Domain)
		if err != nil {
			return nil, fmt.Errorf("cannot list the instances of organization %s: %w", cmd.Domain, err)
		}
		return list, nil
	}
	inst, err := lifecycle.GetInstance(cmd.WorkplaceFqdn)
	if err != nil {
		return nil, err
	}
	return []*instance.Instance{inst}, nil
}

func (cmd Command) applyTo(inst *instance.Instance) error {
	// An instance that displays no banner is a no-op, not a rejection.
	if !inst.HasBannersEnabled() {
		return nil
	}

	// The lock makes the read-then-write of the stored revision atomic across
	// concurrent deliveries and stack processes.
	mu := config.Lock().ReadWrite(inst, "banners")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	stored, err := storedCommand(inst, cmd.Category)
	if err != nil {
		return err
	}
	if stored != nil && cmd.Revision <= stored.Revision {
		log(inst).Infof("%s: ignoring revision %d, not newer than the stored %d",
			cmd.Category, cmd.Revision, stored.Revision)
		return nil
	}

	// Banner before record: a crash in between is healed by the next
	// delivery (Materialize is idempotent). The reverse would record a
	// decision the user never saw.
	if err := Materialize(inst, cmd.Category, cmd.banner(inst.Locale), time.Now()); err != nil {
		return err
	}

	next := cmd.state()
	if stored != nil {
		next.DocID, next.DocRev = stored.DocID, stored.DocRev
		return couchdb.UpdateDoc(inst, next)
	}
	next.DocID = docID(cmd.Category)
	return couchdb.CreateNamedDocWithDB(inst, next)
}

// commandState records the last command accepted for a category, so ordering
// survives clears (which leave no public document) and unchanged decisions.
type commandState struct {
	DocID  string `json:"_id,omitempty"`
	DocRev string `json:"_rev,omitempty"`

	Category string `json:"category"`
	Revision int64  `json:"revision"`
	Clear    bool   `json:"clear"`
	EventID  string `json:"eventId,omitempty"`
}

func (d *commandState) ID() string         { return d.DocID }
func (d *commandState) Rev() string        { return d.DocRev }
func (d *commandState) DocType() string    { return consts.BannerCommands }
func (d *commandState) SetID(id string)    { d.DocID = id }
func (d *commandState) SetRev(rev string)  { d.DocRev = rev }
func (d *commandState) Clone() couchdb.Doc { cloned := *d; return &cloned }

var _ couchdb.Doc = &commandState{}

func (cmd Command) state() *commandState {
	return &commandState{
		Category: cmd.Category,
		Revision: cmd.Revision,
		Clear:    cmd.Clear,
		EventID:  cmd.EventID,
	}
}

// storedCommand returns the state retained for a category, or nil when none
// exists (including a missing database, which is the first command on an instance).
func storedCommand(db prefixer.Prefixer, category string) (*commandState, error) {
	var doc commandState
	err := couchdb.GetDoc(db, consts.BannerCommands, docID(category), &doc)
	if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// banner is the document the command asks for, or nil for a clear.
func (cmd Command) banner(instanceLocale string) *Banner {
	if cmd.Clear {
		return nil
	}
	at := time.Unix(cmd.Timestamp, 0).UTC()
	locale := cmd.locale(instanceLocale)
	b := &Banner{
		BannerID:     cmd.BannerID,
		Category:     cmd.Category,
		Severity:     cmd.Severity,
		Surface:      cmd.Surface,
		Title:        cmd.Title[locale],
		Text:         cmd.Text[locale],
		Lang:         locale,
		CTA:          cmd.CTA.pick(locale),
		SecondaryCTA: cmd.SecondaryCTA.pick(locale),
		Dismissible:  cmd.Dismissible,
		Priority:     cmd.Priority,
		StartsAt:     cmd.StartsAt,
		EndsAt:       cmd.EndsAt,
		Source:       Source{Trigger: TriggerCommand, At: at},
	}
	// Default to the decision time so a redelivery is identical to the original.
	if b.StartsAt == nil {
		startsAt := at
		b.StartsAt = &startsAt
	}
	return b
}

// locale picks one language for the whole banner. A locale the backend only
// half sent is not used, to avoid mixing languages within a single banner.
func (cmd Command) locale(instanceLocale string) string {
	if asked := lang(instanceLocale); cmd.complete(asked) {
		return asked
	}
	return consts.DefaultLocale
}

// complete reports whether every string the document will carry exists in
// the given locale.
func (cmd Command) complete(locale string) bool {
	if cmd.Text[locale] == "" {
		return false
	}
	if len(cmd.Title) > 0 && cmd.Title[locale] == "" {
		return false
	}
	return cmd.CTA.labelled(locale) && cmd.SecondaryCTA.labelled(locale)
}

func (c *CommandCTA) labelled(locale string) bool {
	return c == nil || c.Label[locale] != ""
}

func (c *CommandCTA) pick(locale string) *CTA {
	if c == nil {
		return nil
	}
	return &CTA{Label: c.Label[locale], URL: c.URL}
}

func (cmd Command) validate() error {
	if !categoryFormat.MatchString(cmd.Category) {
		return fmt.Errorf("%w: category %q is not a valid category", ErrInvalidCommand, cmd.Category)
	}
	// The quota category is measured by the stack itself.
	if cmd.Category == CategoryQuota {
		return fmt.Errorf("%w: the %s category is reserved for the stack's own rules",
			ErrInvalidCommand, CategoryQuota)
	}
	if (cmd.Domain == "") == (cmd.WorkplaceFqdn == "") {
		return fmt.Errorf("%w: exactly one of domain and workplaceFqdn is required", ErrInvalidCommand)
	}
	if target := cmd.Domain + cmd.WorkplaceFqdn; !targetFormat.MatchString(target) {
		return fmt.Errorf("%w: %q is not a valid target", ErrInvalidCommand, target)
	}
	if cmd.Revision <= 0 {
		return fmt.Errorf("%w: a positive revision is required", ErrInvalidCommand)
	}
	if cmd.Timestamp <= 0 {
		return fmt.Errorf("%w: timestamp is required", ErrInvalidCommand)
	}
	if _, err := time.Unix(cmd.Timestamp, 0).UTC().MarshalJSON(); err != nil {
		return fmt.Errorf("%w: timestamp must be epoch seconds within the RFC3339 range", ErrInvalidCommand)
	}
	if len(cmd.EventID) > maxEventIDLen {
		return fmt.Errorf("%w: eventId is longer than %d bytes", ErrInvalidCommand, maxEventIDLen)
	}
	// A clear carries no presentation fields.
	if cmd.Clear {
		if cmd.BannerID != "" || cmd.Severity != "" || cmd.Surface != "" ||
			len(cmd.Title) != 0 || len(cmd.Text) != 0 || cmd.CTA != nil || cmd.SecondaryCTA != nil ||
			cmd.Dismissible || cmd.Priority != 0 || cmd.StartsAt != nil || cmd.EndsAt != nil {
			return fmt.Errorf("%w: clear must not carry presentation fields", ErrInvalidCommand)
		}
		return nil
	}

	if !bannerIDFormat.MatchString(cmd.BannerID) {
		return fmt.Errorf("%w: bannerId %q is not a valid identifier", ErrInvalidCommand, cmd.BannerID)
	}
	switch cmd.Severity {
	case SeverityInfo, SeverityWarning, SeverityError:
	default:
		return fmt.Errorf("%w: severity %q is not one of info, warning, error", ErrInvalidCommand, cmd.Severity)
	}
	switch cmd.Surface {
	case SurfaceBanner, SurfaceModal:
	default:
		return fmt.Errorf("%w: surface %q is not one of banner, modal", ErrInvalidCommand, cmd.Surface)
	}
	if cmd.Priority < 0 || cmd.Priority > maxPriority {
		return fmt.Errorf("%w: priority %d is outside 0..%d", ErrInvalidCommand, cmd.Priority, maxPriority)
	}
	startsAt := time.Unix(cmd.Timestamp, 0).UTC()
	if cmd.StartsAt != nil {
		startsAt = *cmd.StartsAt
	}
	for _, at := range []*time.Time{cmd.StartsAt, cmd.EndsAt} {
		if at != nil {
			if _, err := at.MarshalJSON(); err != nil {
				return fmt.Errorf("%w: window must be within the RFC3339 range", ErrInvalidCommand)
			}
		}
	}
	if cmd.EndsAt != nil && !startsAt.Before(*cmd.EndsAt) {
		return fmt.Errorf("%w: startsAt is not before endsAt", ErrInvalidCommand)
	}
	// cozy-client drops a secondary action that has no primary.
	if cmd.SecondaryCTA != nil && cmd.CTA == nil {
		return fmt.Errorf("%w: secondaryCta needs a cta", ErrInvalidCommand)
	}
	if err := cmd.Text.validate("text", maxTextLen); err != nil {
		return err
	}
	if err := cmd.Title.validate("title", maxTitleLen); err != nil {
		return err
	}
	if err := cmd.CTA.validate("cta"); err != nil {
		return err
	}
	if err := cmd.SecondaryCTA.validate("secondaryCta"); err != nil {
		return err
	}
	if !cmd.complete(consts.DefaultLocale) {
		return fmt.Errorf("%w: every text and label is required in the %s fallback locale",
			ErrInvalidCommand, consts.DefaultLocale)
	}
	raw, err := json.Marshal(cmd)
	if err != nil || len(raw) > MaxCommandBytes {
		return fmt.Errorf("%w: command must encode to at most %d JSON bytes", ErrInvalidCommand, MaxCommandBytes)
	}
	return nil
}

// validate checks every locale, not just the one that will be picked.
func (l Localized) validate(field string, max int) error {
	if len(l) > maxLocales {
		return fmt.Errorf("%w: %s carries more than %d locales", ErrInvalidCommand, field, maxLocales)
	}
	for locale, text := range l {
		if len(locale) == 0 || len(locale) > maxLocaleLen {
			return fmt.Errorf("%w: %s locale key must be 1..%d bytes", ErrInvalidCommand, field, maxLocaleLen)
		}
		if len(text) > max {
			return fmt.Errorf("%w: %s is longer than %d bytes in locale %s",
				ErrInvalidCommand, field, max, locale)
		}
	}
	return nil
}

func (c *CommandCTA) validate(field string) error {
	if c == nil {
		return nil
	}
	if len(c.URL) > maxURLLen {
		return fmt.Errorf("%w: %s.url is longer than %d bytes", ErrInvalidCommand, field, maxURLLen)
	}
	if ctaTarget(c.URL) == "" {
		return fmt.Errorf("%w: %s.url is not an absolute https URL", ErrInvalidCommand, field)
	}
	return c.Label.validate(field+".label", maxLabelLen)
}
