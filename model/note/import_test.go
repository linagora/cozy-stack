package note

import (
	"strings"
	"testing"
	"time"

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

func TestParseFileLineOfOpeningBrackets(t *testing.T) {
	specs := model.SchemaSpecFromJSON(DefaultSchemaSpecs())
	schema, err := model.NewSchema(&specs)
	require.NoError(t, err)

	start := time.Now()
	_, err = parseFile(strings.NewReader(strings.Repeat("[", 200_000)), schema)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}
