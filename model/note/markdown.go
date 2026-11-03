package note

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/cozy/cozy-stack/model/note/custom"
	"github.com/cozy/prosemirror-go/markdown"
	"github.com/cozy/prosemirror-go/model"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/util"
)

func markdownSerializer(images []*Image) *markdown.Serializer {
	vanilla := markdown.DefaultSerializer
	nodes := map[string]markdown.NodeSerializerFunc{
		"paragraph": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			for _, mark := range node.Marks {
				if mark.Type.Name == "alignment" {
					switch mark.Attrs["align"] {
					case "center":
						state.Write("[^]{.center}")
					case "end":
						state.Write("[^]{.right}")
					}
				}
			}
			state.RenderInline(node)
			state.CloseBlock(node)
		},
		"text":        vanilla.Nodes["text"],
		"bulletList":  vanilla.Nodes["bullet_list"],
		"orderedList": vanilla.Nodes["ordered_list"],
		"listItem":    vanilla.Nodes["list_item"],
		"decisionList": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			if node.Attrs == nil {
				node.Attrs = map[string]interface{}{}
			}
			node.Attrs["tight"] = true
			state.RenderList(node, "  ", func(_ int) string { return "✍ " })
		},
		"decisionItem": vanilla.Nodes["list_item"],
		"taskList": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			state.RenderList(node, "  ", func(_ int) string { return "- " })
		},
		"taskItem": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			if node.Attrs["state"] == "DONE" {
				state.Write("[X] ")
			} else {
				state.Write("[ ] ")
			}
			state.RenderContent(node)
		},
		"heading":    vanilla.Nodes["heading"],
		"blockquote": vanilla.Nodes["blockquote"],
		"rule":       vanilla.Nodes["horizontal_rule"],
		"hardBreak":  vanilla.Nodes["hard_break"],
		"image":      vanilla.Nodes["image"],
		"codeBlock": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			lang, _ := node.Attrs["language"].(string)
			state.Write("```" + lang + "\n")
			state.Text(node.TextContent(), false)
			state.EnsureNewLine()
			state.Write("```")
			state.CloseBlock(node)
		},
		"panel": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			if typ, ok := node.Attrs["panelType"].(string); ok {
				state.Write(":" + typ + ": ")
			}
			state.RenderContent(node)
		},
		"table": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			var attrs string
			if node.Attrs["layout"] == "wide" {
				attrs += ` layout="wide"`
			} else if node.Attrs["layout"] == "full-width" {
				attrs += ` layout="full-width"`
			}
			if node.Attrs["isNumberColumnEnabled"] == true {
				attrs += " number=true"
			}
			state.Write("________________________________________{.table" + attrs + "}\n\n")
			state.RenderContent(node)
			state.EnsureNewLine()
			state.Write("________________________________________{.tableEnd}\n")
			state.CloseBlock(node)
		},
		"tableRow": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			state.Write("________________________________________{.tableRow}\n\n")
			state.RenderContent(node)
			state.EnsureNewLine()
			state.CloseBlock(node)
		},
		"tableHeader": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			attrs := cellMarkup(node)
			state.Write("____________________{.tableHeader" + attrs + "}\n\n")
			state.RenderContent(node)
		},
		"tableCell": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			attrs := cellMarkup(node)
			state.Write("____________________{.tableCell" + attrs + "}\n\n")
			state.RenderContent(node)
		},
		"status": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			if txt, ok := node.Attrs["text"].(string); ok {
				state.Write("[")
				state.Text(txt)
				state.Write("]")
				color, _ := node.Attrs["color"].(string)
				id, _ := node.Attrs["localId"].(string)
				state.Text(fmt.Sprintf(`{.status color="%s" localId="%s"}`, color, id))
			}
		},
		"date": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			if ts, ok := node.Attrs["timestamp"].(string); ok {
				if seconds, err := strconv.ParseInt(ts, 10, 64); err == nil {
					txt := time.Unix(seconds/1000, 0).Format("2006-01-02")
					state.Write("[")
					state.Text(txt)
					state.Write("]")
					state.Text(fmt.Sprintf(`{.date ts="%s"}`, ts))
				}
			}
		},
		"mediaSingle": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			state.RenderContent(node)
		},
		"media": func(state *markdown.SerializerState, node, _parent *model.Node, _index int) {
			var alt string
			src, _ := node.Attrs["url"].(string)
			for _, img := range images {
				if img.DocID == src {
					alt = img.Name
					img.seen = true
				}
			}
			state.Write(fmt.Sprintf("![%s](%s)\n", state.Esc(alt), state.Esc(src)))
		},
	}
	marks := map[string]markdown.MarkSerializerSpec{
		"em":          vanilla.Marks["em"],
		"strong":      vanilla.Marks["strong"],
		"link":        vanilla.Marks["link"],
		"code":        vanilla.Marks["code"],
		"strike":      {Open: "~~", Close: "~~", ExpelEnclosingWhitespace: true},
		"indentation": {Open: "    ", Close: "", ExpelEnclosingWhitespace: true},
		"breakout":    {Open: "", Close: "", ExpelEnclosingWhitespace: true},
		"underline":   {Open: "[", Close: "]{.underlined}", ExpelEnclosingWhitespace: true},
		"subsup": {
			Open: "[",
			Close: func(state *markdown.SerializerState, mark *model.Mark, parent *model.Node, index int) string {
				typ, _ := mark.Attrs["type"].(string)
				return fmt.Sprintf("]{.%s}", typ)
			},
		},
		"textColor": {
			Open: "[",
			Close: func(state *markdown.SerializerState, mark *model.Mark, parent *model.Node, index int) string {
				color, _ := mark.Attrs["color"].(string)
				return fmt.Sprintf(`]{.color rgb="%s"}`, color)
			},
		},
	}
	return markdown.NewSerializer(nodes, marks)
}

func cellMarkup(node *model.Node) string {
	var attrs string
	if color, ok := node.Attrs["background"].(string); ok && color[0] == '#' {
		attrs += fmt.Sprintf(` background="%s"`, color)
	}
	if span, ok := node.Attrs["rowspan"].(float64); ok && span > 1 {
		attrs += fmt.Sprintf(" rowspan=%d", int(span))
	}
	if span, ok := node.Attrs["colspan"].(float64); ok && span > 1 {
		attrs += fmt.Sprintf(" colspan=%d", int(span))
	}
	return attrs
}

func isTableCell(item *markdown.StackItem) bool {
	name := item.Type.Name
	return name == "tableHeader" || name == "tableCell"
}

// errUnbalancedTable is returned when a table marker arrives where no table
// row is open, instead of closing the document node by mistake.
var errUnbalancedTable = errors.New("unbalanced table marker in markdown")

// closeCellAndRow closes the open cell and its row. It fails, without
// touching the stack, when the cell is not inside a row.
func closeCellAndRow(state *markdown.MarkdownParseState) error {
	n := len(state.Stack)
	if n < 2 || state.Stack[n-2].Type.Name != "tableRow" {
		return errUnbalancedTable
	}
	if _, err := state.CloseNode(); err != nil { // Cell
		return err
	}
	_, err := state.CloseNode() // Row
	return err
}

func cellAttributes(node ast.Node) map[string]interface{} {
	background, ok := node.AttributeString("background")
	if !ok {
		background = nil
	}
	colspan, ok := node.AttributeString("colspan")
	if !ok {
		colspan = 1
	}
	rowspan, ok := node.AttributeString("rowspan")
	if !ok {
		rowspan = 1
	}
	return map[string]interface{}{
		"background": background,
		"colspan":    colspan,
		"rowspan":    rowspan,
	}
}

func markdownNodeMapper() markdown.NodeMapper {
	vanilla := markdown.DefaultNodeMapper
	return markdown.NodeMapper{
		// Blocks
		ast.KindDocument:  vanilla[ast.KindDocument],
		ast.KindParagraph: vanilla[ast.KindParagraph],
		ast.KindHeading:   vanilla[ast.KindHeading],
		ast.KindList: func(state *markdown.MarkdownParseState, node ast.Node, entering bool) error {
			if entering {
				isATaskList := false
				listItem := node.FirstChild()
				if listItem != nil {
					isATaskList = true
					for listItem != nil {
						para := listItem.FirstChild()
						if para == nil {
							isATaskList = false
							break
						}
						checkbox := para.FirstChild()
						if checkbox == nil || checkbox.Kind() != extensionast.KindTaskCheckBox {
							isATaskList = false
							break
						}
						listItem = listItem.NextSibling()
					}
				}
				if isATaskList {
					typ, err := state.Schema.NodeType("taskList")
					if err != nil {
						return err
					}
					state.OpenNode(typ, nil)
					return nil
				}
				return vanilla[ast.KindList](state, node, entering)
			}
			_, err := state.CloseNode()
			return err
		},
		ast.KindListItem: func(state *markdown.MarkdownParseState, node ast.Node, entering bool) error {
			inTaskList := false
			for _, item := range state.Stack {
				if item.Type.Name == "taskList" {
					inTaskList = true
				}
			}
			if inTaskList {
				if entering {
					typ, err := state.Schema.NodeType("taskItem")
					if err != nil {
						return err
					}
					state.OpenNode(typ, nil)
					return nil
				} else {
					// taskItem have their content directly inside them, no paragraphs
					item := state.Top()
					paragraphs := item.Content
					item.Content = nil
					for _, paragraph := range paragraphs {
						item.Content = append(item.Content, paragraph.Content.Content...)
					}
					_, err := state.CloseNode()
					return err
				}
			}
			return vanilla[ast.KindListItem](state, node, entering)
		},
		ast.KindTextBlock:       vanilla[ast.KindTextBlock],
		ast.KindBlockquote:      vanilla[ast.KindBlockquote],
		ast.KindCodeBlock:       vanilla[ast.KindCodeBlock],
		ast.KindFencedCodeBlock: vanilla[ast.KindFencedCodeBlock],
		ast.KindThematicBreak:   vanilla[ast.KindThematicBreak],
		custom.KindDecisionList: markdown.GenericBlockHandler("decisionList"),
		custom.KindDecisionItem: func(state *markdown.MarkdownParseState, node ast.Node, entering bool) error {
			if entering {
				typ, err := state.Schema.NodeType("decisionItem")
				if err != nil {
					return err
				}
				state.OpenNode(typ, nil)
			} else {
				// decisionItem have their content directly inside them, no paragraphs
				item := state.Top()
				paragraphs := item.Content
				item.Content = nil
				for _, paragraph := range paragraphs {
					item.Content = append(item.Content, paragraph.Content.Content...)
				}
				if _, err := state.CloseNode(); err != nil {
					return err
				}
			}
			return nil
		},
		custom.KindPanel: func(state *markdown.MarkdownParseState, node ast.Node, entering bool) error {
			if entering {
				typ, err := state.Schema.NodeType("panel")
				if err != nil {
					return err
				}
				attrs := map[string]interface{}{
					"panelType": node.(*custom.Panel).PanelType,
				}
				state.OpenNode(typ, attrs)
			} else {
				if _, err := state.CloseNode(); err != nil {
					return err
				}
			}
			return nil
		},
		custom.KindTable: func(state *markdown.MarkdownParseState, node ast.Node, entering bool) error {
			tableType, ok := node.AttributeString("class")
			if entering || !ok {
				return nil
			}

			var attrs map[string]interface{}
			switch tableType {
			case "table":
				number, ok := node.AttributeString("number")
				if !ok {
					number = false
				}
				layout, ok := node.AttributeString("layout")
				if !ok {
					layout = "default"
				}
				attrs = map[string]interface{}{
					"__autosize":            false,
					"isNumberColumnEnabled": number,
					"layout":                layout,
				}
			case "tableEnd":
				if isTableCell(state.Top()) {
					if err := closeCellAndRow(state); err != nil {
						return err
					}
				}
				if state.Top().Type.Name != "table" {
					return errUnbalancedTable
				}
				if _, err := state.CloseNode(); err != nil { // Table
					return err
				}
				return nil
			case "tableRow":
				if isTableCell(state.Top()) {
					if err := closeCellAndRow(state); err != nil {
						return err
					}
				}
			case "tableHeader":
				if isTableCell(state.Top()) {
					if _, err := state.CloseNode(); err != nil {
						return err
					}
				}
				attrs = cellAttributes(node)
			case "tableCell":
				if isTableCell(state.Top()) {
					if _, err := state.CloseNode(); err != nil {
						return err
					}
				}
				attrs = cellAttributes(node)
			default:
				return nil
			}
			typ, err := state.Schema.NodeType(tableType.(string))
			if err != nil {
				return err
			}
			state.OpenNode(typ, attrs)
			return nil
		},

		// Inlines
		ast.KindText:     vanilla[ast.KindText],
		ast.KindString:   vanilla[ast.KindString],
		ast.KindAutoLink: vanilla[ast.KindAutoLink],
		ast.KindLink:     vanilla[ast.KindLink],
		ast.KindImage: func(state *markdown.MarkdownParseState, node ast.Node, entering bool) error {
			if entering {
				return nil
			}
			paragraph := state.Pop()
			var text string
			for _, n := range paragraph.Content {
				if n.Text != nil {
					text += *n.Text
				}
			}
			typeMS, err := state.Schema.NodeType("mediaSingle")
			if err != nil {
				return err
			}
			state.OpenNode(typeMS, map[string]interface{}{"layout": "center"})
			typ, err := state.Schema.NodeType("media")
			if err != nil {
				return err
			}
			n := node.(*ast.Image)
			attrs := map[string]interface{}{
				"url":  string(n.Destination),
				"alt":  text,
				"id":   "",
				"type": "external",
			}
			state.OpenNode(typ, attrs)
			if _, err := state.CloseNode(); err != nil { // media
				return err
			}
			if _, err := state.CloseNode(); err != nil { // mediaSingle
				return err
			}
			state.OpenNode(paragraph.Type, paragraph.Attrs)
			return nil
		},
		ast.KindCodeSpan:               vanilla[ast.KindCodeSpan],
		ast.KindEmphasis:               vanilla[ast.KindEmphasis],
		extensionast.KindStrikethrough: vanilla[extensionast.KindStrikethrough],
		extensionast.KindTaskCheckBox: func(state *markdown.MarkdownParseState, node ast.Node, entering bool) error {
			if entering {
				if len(state.Stack) <= 2 {
					return nil
				}
				grandparent := state.Stack[len(state.Stack)-2]
				if grandparent.Type.Name == "taskItem" {
					s := "TODO"
					n := node.(*extensionast.TaskCheckBox)
					if n.IsChecked {
						s = "DONE"
					}
					if grandparent.Attrs == nil {
						grandparent.Attrs = map[string]interface{}{}
					}
					grandparent.Attrs["state"] = s
				}
			}
			return nil
		},
		custom.KindSpan: func(state *markdown.MarkdownParseState, node ast.Node, entering bool) error {
			text := node.(*custom.Span).Value

			var markType, nodeType string
			var attrs map[string]interface{}
			if class, ok := node.AttributeString("class"); ok {
				switch class {
				case "underlined":
					markType = "underline"
				case "sub":
					markType = "subsup"
					attrs = map[string]interface{}{"type": "sub"}
				case "sup":
					markType = "subsup"
					attrs = map[string]interface{}{"type": "sup"}
				case "color":
					if color, ok := node.AttributeString("rgb"); ok {
						markType = "textColor"
						attrs = map[string]interface{}{"color": color}
					}
				case "status":
					if color, ok := node.AttributeString("color"); ok {
						id, _ := node.AttributeString("localId")
						nodeType = "status"
						attrs = map[string]interface{}{
							"color":   color,
							"localId": id,
							"text":    text,
						}
					}
				case "date":
					nodeType = "date"
					ts, _ := node.AttributeString("ts")
					attrs = map[string]interface{}{"timestamp": ts}
				case "center", "right":
					markType = "alignment"
					align := "center"
					if class == "right" {
						align = "end"
					}
					attrs = map[string]interface{}{"align": align}
				}
			}

			if markType != "" {
				typ, err := state.Schema.MarkType(markType)
				if err != nil {
					return err
				}
				mark := typ.Create(attrs)
				if entering {
					state.OpenMark(mark)
					state.AddText(text)
				} else {
					state.CloseMark(mark)
				}
			} else if nodeType != "" {
				if entering {
					typ, err := state.Schema.NodeType(nodeType)
					if err != nil {
						return err
					}
					state.OpenNode(typ, attrs)
				} else {
					if _, err := state.CloseNode(); err != nil {
						return err
					}
				}
			} else {
				if entering {
					state.AddText(text)
				}
				return nil
			}
			return nil
		},
	}
}

func markdownParser() parser.Parser {
	return parser.NewParser(
		parser.WithBlockParsers(
			util.Prioritized(custom.NewTableParser(), 50),
			util.Prioritized(parser.NewSetextHeadingParser(), 100),
			util.Prioritized(parser.NewThematicBreakParser(), 200),
			util.Prioritized(parser.NewListParser(), 300),
			util.Prioritized(parser.NewListItemParser(), 350),
			util.Prioritized(custom.NewDecisionListParser(), 400),
			util.Prioritized(custom.NewDecisionItemParser(), 450),
			util.Prioritized(parser.NewCodeBlockParser(), 500),
			util.Prioritized(parser.NewATXHeadingParser(), 600),
			util.Prioritized(parser.NewFencedCodeBlockParser(), 700),
			util.Prioritized(parser.NewBlockquoteParser(), 800),
			util.Prioritized(custom.NewPanelParser(), 900),
			util.Prioritized(parser.NewParagraphParser(), 1000),
		),
		parser.WithInlineParsers(
			util.Prioritized(extension.NewTaskCheckBoxParser(), 0),
			util.Prioritized(custom.NewSpanParser(), 50),
			util.Prioritized(parser.NewCodeSpanParser(), 100),
			util.Prioritized(parser.NewLinkParser(), 200),
			util.Prioritized(parser.NewAutoLinkParser(), 300),
			util.Prioritized(parser.NewEmphasisParser(), 400),
			util.Prioritized(extension.NewStrikethroughParser(), 500),
		),
		parser.WithParagraphTransformers(
			util.Prioritized(parser.LinkReferenceParagraphTransformer, 100),
		),
	)
}
