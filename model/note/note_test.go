package note

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/prosemirror-go/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkdown(t *testing.T) {
	initial := `# My title

foobar **bold**

:info: this is a panel

✍ this is a decision
✍ and another decision

- [ ] a todo task

- [X] a done task`

	schemaSpecs := DefaultSchemaSpecs()
	specs := model.SchemaSpecFromJSON(schemaSpecs)
	schema, err := model.NewSchema(&specs)
	require.NoError(t, err)

	node, err := parseFile(strings.NewReader(initial), schema)
	require.NoError(t, err)

	md := markdownSerializer(nil).Serialize(node)
	assert.Equal(t, initial, md)
}

func TestText(t *testing.T) {
	initial := `# My title

hello **world**

- foo
- bar
- baz

✍ This is a decision
✍ and another decision

- [ ] A todo task
- [X] a done task`

	expected := `My title

hello world

- foo

- bar

- baz

This is a decision

and another decision

- A todo task

- a done task`

	schemaSpecs := DefaultSchemaSpecs()
	specs := model.SchemaSpecFromJSON(schemaSpecs)
	schema, err := model.NewSchema(&specs)
	require.NoError(t, err)

	node, err := parseFile(strings.NewReader(initial), schema)
	require.NoError(t, err)

	md := textSerializer().Serialize(node)
	assert.Equal(t, expected, md)
}

func TestMarkdownTableAttributes(t *testing.T) {
	specs := model.SchemaSpecFromJSON(DefaultSchemaSpecs())
	schema, err := model.NewSchema(&specs)
	require.NoError(t, err)

	for name, input := range map[string]string{
		"malformed": "}\n__________{. ackground=\"#ff}000\" colspatableCelad\n\n" +
			"________________________________________{.tableRow}\n\n" +
			"____________________{.tableCell rowspan=2}",
		"unterminated":  "__________{A\n__________{",
		"missing value": "__________{background=\n\n__________{.tableRow}",
	} {
		t.Run(name, func(t *testing.T) {
			node, err := parseFile(strings.NewReader(input), schema)
			require.NoError(t, err)
			require.NotNil(t, node)
		})
	}

	t.Run("valid table", func(t *testing.T) {
		input := "________________________________________{.table}\n\n" +
			"________________________________________{.tableRow}\n\n" +
			"____________________{.tableCell background=\"#ff0000\" rowspan=2}\n\n" +
			"hello\n\n________________________________________{.tableEnd}\n"
		node, err := parseFile(strings.NewReader(input), schema)
		require.NoError(t, err)
		assert.Equal(t, input, markdownSerializer(nil).Serialize(node))
	})
}

func TestApplyStepThatPanicsInProsemirror(t *testing.T) {
	doc := &Document{
		SchemaSpec: DefaultSchemaSpecs(),
		RawContent: map[string]interface{}{"type": "doc", "content": []interface{}{
			map[string]interface{}{"type": "heading", "attrs": map[string]interface{}{"level": 1.0},
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "Title"}}},
			map[string]interface{}{"type": "bulletList", "content": []interface{}{
				map[string]interface{}{"type": "listItem", "content": []interface{}{map[string]interface{}{"type": "paragraph",
					"content": []interface{}{map[string]interface{}{"type": "text", "text": "foo"}}}}},
				map[string]interface{}{"type": "listItem", "content": []interface{}{map[string]interface{}{"type": "paragraph",
					"content": []interface{}{map[string]interface{}{"type": "text", "text": "bar"}}}}},
			}},
		}},
	}
	var step Step
	require.NoError(t, json.Unmarshal([]byte(`{"stepType":"replaceAround","from":18,"to":19,"gapFrom":18,"gapTo":19,"insert":-1,`+
		`"slice":{"content":[{"type":"paragraph","content":[{"type":"text","text":"ab"}]}],"openStart":1,"openEnd":1},"structure":true}`), &step))

	inst := &instance.Instance{Domain: "notes.example.net"}
	require.NotPanics(t, func() {
		assert.ErrorIs(t, apply(inst, doc, []Step{step}), ErrCannotApply)
	})
	assert.Equal(t, int64(0), doc.Version)
}
