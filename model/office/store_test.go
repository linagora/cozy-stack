package office_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/office"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreSessionSharesNoMemory(t *testing.T) {
	config.UseTestFile(t)
	db := prefixer.NewPrefixer(0, "store.example.net", "store-example-net")
	store := office.GetStore()

	md5sum := []byte{1, 2, 3}
	key, err := store.AddDoc(db, office.ConflictDetector{ID: "file-1", Rev: "1-abc", MD5Sum: md5sum})
	require.NoError(t, err)

	// Changing what was given to the store does not change the session.
	md5sum[0] = 9
	got, err := store.GetDoc(db, key)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2, 3}, got.MD5Sum)

	// Neither does changing what the store returned.
	got.MD5Sum[0] = 8
	got.Rev = "2-def"
	again, err := store.GetDoc(db, key)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2, 3}, again.MD5Sum)
	assert.Equal(t, "1-abc", again.Rev)
}
