package recents_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicIDHidesTheResourceKey(t *testing.T) {
	key := "9b2e4c0a7f1d4e8b9c3a5d6e7f801234:file-1"

	id := recents.PublicID(key)
	assert.Len(t, id, 32)
	assert.Equal(t, id, recents.PublicID(key), "the public ID of a row is stable")
	assert.NotEqual(t, id, recents.PublicID(key+"0"))
	assert.NotContains(t, id, "9b2e4c0a7f1d4e8b9c3a5d6e7f801234")
}

func TestEnabledReadsTheInstanceFlags(t *testing.T) {
	for _, tc := range []struct {
		value    interface{}
		expected bool
	}{
		{true, true},
		{false, false},
		{nil, false},
		{"true", false},
	} {
		inst := &instance.Instance{
			DocID:        uuid.Must(uuid.NewV7()).String(),
			FeatureFlags: map[string]interface{}{"drive.recents.enabled": tc.value},
		}
		assert.Equal(t, tc.expected, recents.Enabled(inst), "flag value %#v", tc.value)
	}
}

func TestEnabledReadsTheOtherFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("CouchDB is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	contextName := "recents-" + uuid.Must(uuid.NewV7()).String()
	inst := &instance.Instance{
		DocID:       uuid.Must(uuid.NewV7()).String(),
		ContextName: contextName,
	}
	assert.False(t, recents.Enabled(inst), "the flag is off by default")

	flags := &couchdb.JSONDoc{
		Type: consts.Settings,
		M: map[string]interface{}{
			"_id": consts.ContextFlagsSettingsID + "." + contextName,
			"drive.recents.enabled": []interface{}{
				map[string]interface{}{"ratio": 1.0, "value": true},
			},
		},
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(prefixer.GlobalPrefixer, flags))
	t.Cleanup(func() { _ = couchdb.DeleteDoc(prefixer.GlobalPrefixer, flags) })

	assert.True(t, recents.Enabled(inst), "a change of the context flag applies at once")

	inst.FeatureFlags = map[string]interface{}{"drive.recents.enabled": false}
	assert.False(t, recents.Enabled(inst), "the flag of the instance wins over the context")
}
