package settings

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckInstanceSettingsDropsUnchangedValues(t *testing.T) {
	// A legacy locale or context sent back unchanged is left out of the
	// patch, so a conflict retry cannot write it over a concurrent repair.
	inst := &instance.Instance{Locale: "xx", ContextName: "old-context"}
	doc := &couchdb.JSONDoc{M: map[string]interface{}{
		"locale": "xx", "context": "old-context", "tz": "Mars/Olympus",
	}}
	current := &couchdb.JSONDoc{M: map[string]interface{}{"tz": "Mars/Olympus"}}
	require.NoError(t, checkInstanceSettings(inst, doc, current))
	assert.NotContains(t, doc.M, "locale")
	assert.NotContains(t, doc.M, "context")
	assert.Equal(t, "Mars/Olympus", doc.M["tz"]) // the settings doc is replaced whole
}
