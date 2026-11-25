package custom

import (
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

func parseMarkdown(source string) ast.Node {
	p := parser.NewParser(
		parser.WithBlockParsers(
			util.Prioritized(NewTableParser(), 50),
			util.Prioritized(parser.NewParagraphParser(), 1000),
		),
		parser.WithInlineParsers(
			util.Prioritized(NewSpanParser(), 50),
		),
	)
	return p.Parse(text.NewReader([]byte(source)))
}

func findNode(doc ast.Node, kind ast.NodeKind) ast.Node {
	var found ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && n.Kind() == kind {
			found = n
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return found
}

func TestFlatAttributes(t *testing.T) {
	doc := parseMarkdown(`[foo]{.mention id="a\"}b" rgb="#ff0000"}` + "\n")
	span := findNode(doc, KindSpan)
	require.NotNil(t, span)
	class, _ := span.AttributeString("class")
	assert.Equal(t, "mention", class)
	id, _ := span.AttributeString("id")
	assert.Equal(t, `a"}b`, id)
	rgb, _ := span.AttributeString("rgb")
	assert.Equal(t, "#ff0000", rgb)

	doc = parseMarkdown(`__________{.table layout="wide" number=true}` + "\n")
	table := findNode(doc, KindTable)
	require.NotNil(t, table)
	class, _ = table.AttributeString("class")
	assert.Equal(t, "table", class)
	layout, _ := table.AttributeString("layout")
	assert.Equal(t, "wide", layout)
	number, _ := table.AttributeString("number")
	assert.Equal(t, true, number)
}

func TestNestedAttributesAreRejected(t *testing.T) {
	// Without a guard, goldmark recurses twice per '[' and needs far more
	// stack than this limit, which ends with a fatal stack overflow.
	defer debug.SetMaxStack(debug.SetMaxStack(1 << 20))

	nested := strings.Repeat("[", 20000)
	doc := parseMarkdown("[x]{a=" + nested + "\n")
	span := findNode(doc, KindSpan)
	require.NotNil(t, span)
	assert.Empty(t, span.Attributes())

	doc = parseMarkdown("__________{a=" + nested + "\n")
	table := findNode(doc, KindTable)
	require.NotNil(t, table)
	assert.Empty(t, table.Attributes())

	doc = parseMarkdown(`[x]{a={b=1}}` + "\n")
	span = findNode(doc, KindSpan)
	require.NotNil(t, span)
	assert.Empty(t, span.Attributes())

	doc = parseMarkdown("[x]{a=" + strings.Repeat("b", maxAttributesLength) + "}\n")
	span = findNode(doc, KindSpan)
	require.NotNil(t, span)
	assert.Empty(t, span.Attributes())
}
