package custom

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// A Span struct represents a span of text with attributes.
type Span struct {
	ast.BaseInline
	Value string
}

// Dump implements Node.Dump.
func (n *Span) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// KindSpan is a NodeKind of the Span node.
var KindSpan = ast.NewNodeKind("Span")

// Kind implements Node.Kind.
func (n *Span) Kind() ast.NodeKind {
	return KindSpan
}

// NewSpan returns a new Span node.
func NewSpan(value string) *Span {
	return &Span{
		BaseInline: ast.BaseInline{},
		Value:      value,
	}
}

type spanParser struct{}

var defaultSpanParser = &spanParser{}

// NewSpanParser returns a new InlineParser that can parse spans with
// attributes, like [foo bar]{.myClass}.
// See https://talk.commonmark.org/t/consistent-attribute-syntax/272.
// This parser must take precedence over the parser.LinkParser.
func NewSpanParser() parser.InlineParser {
	return defaultSpanParser
}

func (s *spanParser) Trigger() []byte {
	return []byte{'['}
}

func (s *spanParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	for pos := 1; pos < len(line); pos++ {
		// The next [ gets its own call: scanning past it is quadratic on a line of [.
		if line[pos] == '[' {
			return nil
		}
		if line[pos] == ']' {
			pos++
			if pos >= len(line) || line[pos] != '{' {
				return nil
			}
			block.Advance(pos)
			span := NewSpan(string(line[1 : pos-1]))
			if attrs, ok := parser.ParseAttributes(block); ok {
				for _, attr := range attrs {
					if value, ok := attr.Value.([]byte); ok {
						span.SetAttribute(attr.Name, string(value))
					} else {
						span.SetAttribute(attr.Name, attr.Value)
					}
				}
			}
			return span
		}
	}
	return nil
}

func (s *spanParser) CloseBlock(parent ast.Node, pc parser.Context) {
	// nothing to do
}
