package permission

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffSameSets(t *testing.T) {
	set1 := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(GET),
			Type:   "io.cozy.files",
			Values: []string{"io.cozy.files.music-dir"},
		},
	}

	set2 := Set{
		Rule{
			Title:  "rule2",
			Verbs:  Verbs(GET),
			Type:   "io.cozy.files",
			Values: []string{"io.cozy.files.music-dir"},
		},
	}

	d := Diff(set1, set2)
	assert.True(t, set1.HasSameRules(d))
}

func TestDiffNotSameSets(t *testing.T) {
	set1 := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(GET),
			Type:   "io.cozy.files",
			Values: []string{"io.cozy.files.music-dir"},
		},
	}

	set2 := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(GET, POST),
			Type:   "io.cozy.files",
			Values: []string{"io.cozy.files.music-dir"},
		},
	}

	d := Diff(set1, set2)
	assert.False(t, set1.HasSameRules(d))

	expectedSet := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(POST), // Only the POST has been added
			Type:   "io.cozy.files",
			Values: []string{}, // No addition has been made
		},
	}

	assert.Equal(t, expectedSet, d)
}

func TestDiffMultipleRules(t *testing.T) {
	set1 := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(GET),
			Type:   "io.cozy.files",
			Values: []string{"io.cozy.files.music-dir"},
		},
		Rule{
			Title:  "rule2",
			Verbs:  Verbs(PATCH, DELETE),
			Type:   "io.cozy.foobar",
			Values: []string{"io.cozy.files.music-dir"},
		},
	}

	set2 := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(GET, POST),
			Type:   "io.cozy.files",
			Values: []string{"io.cozy.files.music-dir"},
		},
		Rule{
			Title:  "rule2",
			Verbs:  Verbs(PATCH, DELETE, GET, POST),
			Type:   "io.cozy.foobar",
			Values: []string{"io.cozy.files.music-dir", "io.cozy.files.foobar-dir"},
		},
	}

	d := Diff(set1, set2)
	assert.False(t, set1.HasSameRules(d))

	expectedSet := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(POST), // Only the POST has been added
			Type:   "io.cozy.files",
			Values: []string{}, // No addition has been made
		},
		Rule{
			Title:  "rule2",
			Verbs:  Verbs(GET, POST), // GET & POST has been added
			Type:   "io.cozy.foobar",
			Values: []string{"io.cozy.files.foobar-dir"}, // A new folder was added
		},
	}
	assert.Equal(t, expectedSet, d)
}

func TestDiffNotSameSetsNewRule(t *testing.T) {
	set1 := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(GET),
			Type:   "io.cozy.files",
			Values: []string{"io.cozy.files.music-dir"},
		},
	}

	set2 := Set{
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(GET, POST),
			Type:   "io.cozy.files",
			Values: []string{"io.cozy.files.music-dir"},
		},
		Rule{
			Title: "myNewRule",
			Verbs: Verbs(GET, POST),
			Type:  "io.cozy.ducky",
		},
	}

	d := Diff(set1, set2)
	assert.False(t, set1.HasSameRules(d))

	expectedSet := Set{
		Rule{
			Title: "myNewRule",
			Verbs: Verbs(GET, POST),
			Type:  "io.cozy.ducky",
		},
		Rule{
			Title:  "rule1",
			Verbs:  Verbs(POST), // Only the POST has been added
			Type:   "io.cozy.files",
			Values: []string{}, // No addition has been made
		},
	}

	assert.Equal(t, expectedSet, d)
}

func TestSetUnmarshalJSONEdgeCases(t *testing.T) {
	var s Set
	require.NoError(t, json.Unmarshal([]byte(`null`), &s))
	assert.NotNil(t, s)
	assert.Len(t, s, 0)

	assert.Error(t, json.Unmarshal([]byte(`[]`), &s))
	assert.Error(t, json.Unmarshal([]byte(`{"a":"x"}`), &s))

	// A duplicated title gives a rule per occurrence, all with the last value.
	require.NoError(t, json.Unmarshal([]byte(`{"f":{"type":"io.cozy.files"},"c":{"type":"io.cozy.contacts"},"f":{"type":"io.cozy.notes"}}`), &s))
	require.Len(t, s, 3)
	assert.Equal(t, []string{"f", "c", "f"}, []string{s[0].Title, s[1].Title, s[2].Title})
	assert.Equal(t, []string{"io.cozy.notes", "io.cozy.contacts", "io.cozy.notes"}, []string{s[0].Type, s[1].Type, s[2].Type})
}

func BenchmarkSetUnmarshalJSON(b *testing.B) {
	values := make([]string, 200)
	for i := range values {
		values[i] = fmt.Sprintf(`"file-id-%03d-0123456789abcdef"`, i)
	}
	sets := map[string][]byte{
		"rules=22":   nil,
		"values=200": []byte(`{"files":{"type":"io.cozy.files","verbs":["GET"],"values":[` + strings.Join(values, ",") + `]}}`),
	}
	var rules []string
	for i := 0; i < 22; i++ {
		rules = append(rules, fmt.Sprintf(`"rule%d":{"type":"io.cozy.doctype%d","description":"Required for something","verbs":["GET","POST"],"selector":"worker","values":["a","b"]}`, i, i))
	}
	sets["rules=22"] = []byte(`{` + strings.Join(rules, ",") + `}`)
	for name, j := range sets {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var s Set
				if err := json.Unmarshal(j, &s); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
