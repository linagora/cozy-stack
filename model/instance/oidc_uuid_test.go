package instance_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRememberOIDCUUID(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()

	instance.RememberOIDCUUID(inst, map[string]interface{}{"sub": "alice"})
	assert.Empty(t, inst.OIDCUUID)
	instance.RememberOIDCUUID(inst, map[string]interface{}{"uuid": "not a uuid"})
	assert.Empty(t, inst.OIDCUUID)

	instance.RememberOIDCUUID(inst, map[string]interface{}{"uuid": "6208AD54-5683-1041-8C8F-C9E6DCCFD4AC"})
	assert.Equal(t, "6208ad54-5683-1041-8c8f-c9e6dccfd4ac", inst.OIDCUUID)
	saved, err := instance.Get(inst.Domain)
	require.NoError(t, err)
	assert.Equal(t, "6208ad54-5683-1041-8c8f-c9e6dccfd4ac", saved.OIDCUUID)
}
