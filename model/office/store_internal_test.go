package office

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemStoreUpdateIgnoresAnExpiredSession(t *testing.T) {
	s := newMemStore().(*memStore)
	db := prefixer.NewPrefixer(0, "expired.example.net", "expired-example-net")

	key, err := s.AddDoc(db, ConflictDetector{ID: "file-1", Rev: "1-abc"})
	require.NoError(t, err)
	// The session expired, and the cleaner has not removed it yet.
	s.vals[docKey(db, key)].exp = time.Now().Add(-time.Minute)

	require.NoError(t, s.AddParticipant(db, key, "alice.example.net"))
	require.NoError(t, s.MoveToRevision(db, key, "file-1", "2-def", nil))

	got, err := s.GetDoc(db, key)
	require.NoError(t, err)
	assert.Nil(t, got, "an update must not revive an expired session")
}
