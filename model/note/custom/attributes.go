package custom

import (
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// maxAttributesLength is the maximal length in bytes of a block of attributes
// like {.myClass rgb=#ff0000}.
const maxAttributesLength = 1024

// parseFlatAttributes parses a block of attributes, starting at the '{' of
// the current line of the reader. Only flat attributes are accepted: the
// goldmark parser recurses for each nested '[' or '{', without depth limit,
// and a crafted note can exhaust the stack. When the block is too long, has
// nested values, or is not closed on the current line, it is rejected without
// consuming the reader.
func parseFlatAttributes(reader text.Reader) (parser.Attributes, bool) {
	line, _ := reader.PeekLine()
	if len(line) == 0 || line[0] != '{' {
		return nil, false
	}
	inString := false
	for i := 1; i < len(line) && i < maxAttributesLength; i++ {
		c := line[i]
		if inString {
			if c == '\\' {
				i++
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[', '{':
			return nil, false
		case '}':
			return parser.ParseAttributes(reader)
		}
	}
	return nil, false
}
