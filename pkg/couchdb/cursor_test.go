package couchdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCursor(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	t.Run("StartKeyCursor", func(t *testing.T) {
		req1 := &ViewRequest{
			Key: []string{"A", "B"},
		}

		c1 := NewKeyCursor(10, []string{"A", "B"}, "last-result-id")

		c1.ApplyTo(req1)
		assert.Nil(t, req1.Key)
		assert.Equal(t, []string{"A", "B"}, req1.StartKey)
		assert.Equal(t, "last-result-id", req1.StartKeyDocID)
		assert.Equal(t, 11, req1.Limit)

		c2 := NewKeyCursor(3, nil, "")

		res := &ViewResponse{
			Rows: []*ViewResponseRow{
				{Key: []string{"A", "B"}, ID: "resultA"},
				{Key: []string{"A", "B"}, ID: "resultB"},
				{Key: []string{"A", "B"}, ID: "resultC"},
				{Key: []string{"A", "B"}, ID: "resultD"},
			},
		}

		c2.UpdateFrom(res)
		assert.Len(t, res.Rows, 3)
		assert.Equal(t, []string{"A", "B"}, c2.(*StartKeyCursor).NextKey)
		assert.Equal(t, "resultD", c2.(*StartKeyCursor).NextDocID)
	})
}

func TestStartKeyCursorStaysInRange(t *testing.T) {
	tests := []struct {
		name  string
		req   ViewRequest
		key   interface{}
		keeps bool
	}{
		{
			name:  "range, key inside",
			req:   ViewRequest{StartKey: []string{"dir", ""}, EndKey: []string{"dir", MaxString}},
			key:   []interface{}{"dir", "file", "b"},
			keeps: true,
		},
		{
			name: "range, key before the range",
			req:  ViewRequest{StartKey: []string{"dir", ""}, EndKey: []string{"dir", MaxString}},
			key:  []interface{}{""},
		},
		{
			name: "range, key of another prefix",
			req:  ViewRequest{StartKey: []string{"dir", ""}, EndKey: []string{"dir", MaxString}},
			key:  []interface{}{"other", "file", "b"},
		},
		{
			name:  "single key, same key",
			req:   ViewRequest{Key: [2]interface{}{"doctype", "shared-by-link"}},
			key:   []interface{}{"doctype", "shared-by-link"},
			keeps: true,
		},
		{
			name: "single key, lower key",
			req:  ViewRequest{Key: [2]interface{}{"doctype", "shared-by-link"}},
			key:  []interface{}{},
		},
		{
			name:  "descending range, key inside",
			req:   ViewRequest{StartKey: []string{"a", "b", MaxString}, EndKey: []string{"a", "b"}},
			key:   []interface{}{"a", "b", "c"},
			keeps: true,
		},
		{
			name: "descending range, key outside",
			req:  ViewRequest{StartKey: []string{"a", "b", MaxString}, EndKey: []string{"a", "b"}},
			key:  []interface{}{"a", "x", "c"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			NewKeyCursor(10, tc.key, "id").ApplyTo(&req)
			if tc.keeps {
				assert.Equal(t, tc.key, req.StartKey)
				assert.Equal(t, "id", req.StartKeyDocID)
			} else {
				assert.NotEqual(t, tc.key, req.StartKey)
				assert.Empty(t, req.StartKeyDocID)
			}
		})
	}
}
