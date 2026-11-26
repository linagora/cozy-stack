package custom

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type TableType int

// A Table struct represents a table in atlaskit.
type Table struct {
	ast.BaseBlock
}

// Dump implements Node.Dump.
func (n *Table) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// KindTable is a NodeKind of the Table node.
var KindTable = ast.NewNodeKind("Table")

// Kind implements Node.Kind.
func (n *Table) Kind() ast.NodeKind {
	return KindTable
}

// NewTable returns a new Table node.
func NewTable() *Table {
	return &Table{
		BaseBlock: ast.BaseBlock{},
	}
}

type tableParser struct{}

var defaultTableParser = &tableParser{}

// NewTableParser returns a new BlockParser that parses tables.
// This parser must take precedence over the parser.ThematicBreakParser.
func NewTableParser() parser.BlockParser {
	return defaultTableParser
}

func (b *tableParser) Trigger() []byte {
	return []byte{'_'}
}

func (b *tableParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	w, pos := util.IndentWidth(line, reader.LineOffset())
	if w > 3 {
		return nil, parser.NoChildren
	}
	count := 0
	for i := pos; i < len(line); i++ {
		c := line[i]
		if c == '{' {
			break
		}
		if c != '_' {
			count = 0
			break
		}
		count++
	}
	if count < 10 {
		return nil, parser.NoChildren
	}
	reader.Advance(count)
	node := NewTable()
	// Malformed attributes must not advance the document reader across lines.
	line, _ = reader.PeekLine()
	// Goldmark recurses on each nested '[' or '{' without depth limit:
	// only let it see a bounded slice of the line.
	attributesReader := text.NewReader(line[:min(len(line), maxAttributesLength)])
	if attrs, ok := parser.ParseAttributes(attributesReader); ok {
		_, position := attributesReader.Position()
		reader.Advance(position.Start)
		for _, attr := range attrs {
			if value, ok := attr.Value.([]byte); ok {
				node.SetAttribute(attr.Name, string(value))
			} else {
				node.SetAttribute(attr.Name, attr.Value)
			}
		}
	}
	return node, parser.HasChildren
}

func (b *tableParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	return parser.Close
}

func (b *tableParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {
	// nothing to do
}

func (b *tableParser) CanInterruptParagraph() bool {
	return true
}

func (b *tableParser) CanAcceptIndentedLine() bool {
	return false
}
