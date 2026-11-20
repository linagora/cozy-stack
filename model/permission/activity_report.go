package permission

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

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
