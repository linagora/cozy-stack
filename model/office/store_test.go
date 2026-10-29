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

func TestStoreMoveToRevisionMovesTheFileIndex(t *testing.T) {
	config.UseTestFile(t)
	db := prefixer.NewPrefixer(0, "move.example.net", "move-example-net")
	store := office.GetStore()

	key, err := store.AddDoc(db, office.ConflictDetector{ID: "original", Rev: "1-abc"})
	require.NoError(t, err)
	require.NoError(t, store.AddParticipant(db, key, "alice.example.net"))

	// A conflict moves the session to the copy of the file.
	require.NoError(t, store.MoveToRevision(db, key, "copy", "1-def", []byte{4, 5, 6}))

	got, err := store.GetDoc(db, key)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "copy", got.ID)
	assert.Equal(t, "1-def", got.Rev)
	assert.Equal(t, []byte{4, 5, 6}, got.MD5Sum)
	assert.Equal(t, []string{"alice.example.net"}, got.Participants())

	secret, err := store.GetSecretByID(db, "copy")
	require.NoError(t, err)
	assert.Equal(t, key, secret, "the copy points to the session")
	secret, err = store.GetSecretByID(db, "original")
	require.NoError(t, err)
	assert.Empty(t, secret, "the original file no longer points to the session")
}
