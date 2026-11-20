package permission

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
)

// UnmarshalJSON distinguishes omission from an explicit boolean and rejects null.
func (p *Permission) UnmarshalJSON(data []byte) error {
	type plain Permission
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == json.Delim('{') {
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				return err
			}
			if strings.EqualFold(key.(string), "activity_report_enabled") && string(value) != "true" && string(value) != "false" {
				return jsonapi.InvalidAttribute("activity_report_enabled", errors.New("must be a boolean"))
			}
		}
	}
	return json.Unmarshal(data, (*plain)(p))
}

// ActivityReportExpiry returns the expiry of an opted-in file link. It works
// on expired permissions, which the report generator reads directly from CouchDB.
func (p *Permission) ActivityReportExpiry() (time.Time, bool) {
	if p.Type != TypeShareByLink || p.ActivityReportEnabled == nil || !*p.ActivityReportEnabled || len(p.Codes) == 0 {
		return time.Time{}, false
	}
	expiry, _ := p.ExpiresAt.(string)
	at, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil || at.IsZero() {
		return time.Time{}, false
	}
	for _, rule := range p.Permissions {
		if rule.Type == consts.Files {
			return at.UTC(), true
		}
	}
	return time.Time{}, false
}
