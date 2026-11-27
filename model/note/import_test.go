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

	// At the limit, the rest of the line is kept as text. A list and its item
	// are 2 levels, so 25 lists fit in maxNesting.
	doc, err = parseFile(strings.NewReader(strings.Repeat("- ", maxNesting)+"tail"), schema)
	require.NoError(t, err)
	assert.Equal(t, maxNesting/2, strings.Count(doc.String(), "bulletList("))
	assert.Equal(t, strings.Repeat("- ", maxNesting/2)+"tail", doc.TextContent())

	// A list with two items at the limit must not stop the next list.
	quote := strings.Repeat(">", maxNesting-1)
	doc, err = parseFile(strings.NewReader(quote+" - a\n"+quote+" - b\n\n- outside"), schema)
	require.NoError(t, err)
	last, err := doc.Child(doc.ChildCount() - 1)
	require.NoError(t, err)
	assert.Equal(t, "bulletList", last.Type.Name)
}

func TestParseFileTableCellMarkerInTaskItem(t *testing.T) {
	specs := model.SchemaSpecFromJSON(DefaultSchemaSpecs())
	schema, err := model.NewSchema(&specs)
	require.NoError(t, err)

	node := func(typ string, children ...interface{}) map[string]interface{} {
		n := map[string]interface{}{"type": typ}
		if len(children) > 0 {
			n["content"] = children
		}
		return n
	}
	doc := &Document{SchemaSpec: DefaultSchemaSpecs(), RawContent: node("doc",
		node("taskList", node("taskItem", node("tableHeader", node("tableHeader", node("codeBlock"))))))}
	md, err := doc.Markdown(nil)
	require.NoError(t, err)

	require.NotPanics(t, func() {
		_, err = parseFile(strings.NewReader(string(md)), schema)
	}, string(md))
	assert.ErrorIs(t, err, errUnbalancedTable)
}
