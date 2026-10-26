package recents_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/recents"
	"github.com/stretchr/testify/assert"
)

func TestPublicIDHidesTheResourceKey(t *testing.T) {
	key := "9b2e4c0a7f1d4e8b9c3a5d6e7f801234:file-1"

	id := recents.PublicID(key)
	assert.Len(t, id, 32)
	assert.Equal(t, id, recents.PublicID(key), "the public ID of a row is stable")
	assert.NotEqual(t, id, recents.PublicID(key+"0"))
	assert.NotContains(t, id, "9b2e4c0a7f1d4e8b9c3a5d6e7f801234")
}
