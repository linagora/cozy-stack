package note

import (
	"strings"
	"testing"

	"github.com/cozy/prosemirror-go/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFileUnbalancedTableMarkers(t *testing.T) {
	specs := model.SchemaSpecFromJSON(DefaultSchemaSpecs())
	schema, err := model.NewSchema(&specs)
	require.NoError(t, err)

	for _, md := range []string{
		"__________{.tableEnd}\n",
		"__________{.tableCell}\n\n__________{.tableRow}\n",
		"__________{.tableCell}\n\n__________{.tableEnd}\n",
	} {
		require.NotPanics(t, func() {
			_, err := parseFile(strings.NewReader(md), schema)
			assert.Error(t, err)
		}, md)
	}
}
