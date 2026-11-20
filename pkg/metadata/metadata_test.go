package metadata

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdatedByAppPreservesInstance(t *testing.T) {
	md := &CozyMetadata{UpdatedByApps: []*UpdatedByAppEntry{{Slug: "drive", Instance: "member.example"}}}
	require.NoError(t, md.UpdatedByApp("drive", "2"))
	require.Equal(t, "member.example", md.UpdatedByApps[0].Instance)
	require.Equal(t, "2", md.UpdatedByApps[0].Version)
	require.NoError(t, md.UpdatedByApp("other", "1"))
	require.Empty(t, md.UpdatedByApps[1].Instance)
}
