package permission

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
)

// UnmarshalJSON rejects explicit null and non-booleans for the report option.
// A pointer distinguishes an omitted PATCH attribute from an explicit false.
func (p *Permission) UnmarshalJSON(data []byte) error {
	type plain Permission
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if value, ok := fields["activity_report_enabled"]; ok && string(value) != "true" && string(value) != "false" {
		return jsonapi.InvalidAttribute("activity_report_enabled", errors.New("must be a boolean"))
	}
	return json.Unmarshal(data, (*plain)(p))
}

// ActivityReportExpiry identifies opted-in file links, including expired ones.
// Normalizing the instant makes equivalent RFC3339 offsets one report.
func (p *Permission) ActivityReportExpiry() (time.Time, bool) {
	if p.Type != TypeShareByLink || p.ActivityReportEnabled == nil || !*p.ActivityReportEnabled || len(p.Codes) == 0 {
		return time.Time{}, false
	}
	files := false
	for _, rule := range p.Permissions {
		if rule.Type == consts.Files {
			files = true
		}
	}
	if !files {
		return time.Time{}, false
	}
	var at time.Time
	switch value := p.ExpiresAt.(type) {
	case string:
		var err error
		at, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, false
		}
	case time.Time:
		at = value
	case *time.Time:
		if value != nil {
			at = *value
		}
	}
	return at.UTC(), !at.IsZero()
}
