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

func TestParseFileDeepNesting(t *testing.T) {
	specs := model.SchemaSpecFromJSON(DefaultSchemaSpecs())
	schema, err := model.NewSchema(&specs)
	require.NoError(t, err)

	// goldmark rescans the line for each level of nesting, without a limit.
	start := time.Now()
	for _, md := range []string{
		strings.Repeat(">", 100_000),
		strings.Repeat("- ", 50_000) + "x",
		strings.Repeat(":info: ", 20_000) + "x",
	} {
		_, err := parseFile(strings.NewReader(md), schema)
		require.NoError(t, err)
	}
	assert.Less(t, time.Since(start), 5*time.Second)

	// Lists nested less deeply keep their structure.
	doc, err := parseFile(strings.NewReader(strings.Repeat("- ", 20)+"x"), schema)
	require.NoError(t, err)
	assert.Equal(t, 20, strings.Count(doc.String(), "bulletList("))
	assert.Equal(t, "x", doc.TextContent())
}
