package couchdb

import (
	"encoding/json"
	"reflect"
)

// A Cursor holds a reference to a page in a couchdb View
type Cursor interface {
	HasMore() bool
	ApplyTo(req *ViewRequest)
	UpdateFrom(res *ViewResponse)
}

// NewKeyCursor returns a new key based Cursor pointing to
// the given start_key & startkey_docid
func NewKeyCursor(limit int, key interface{}, id string) Cursor {
	return &StartKeyCursor{
		baseCursor: &baseCursor{Limit: limit},
		NextKey:    key,
		NextDocID:  id,
	}
}

// NewSkipCursor returns a new skip based Cursor pointing to
// the page after skip items
func NewSkipCursor(limit, skip int) Cursor {
	return &SkipCursor{
		baseCursor: &baseCursor{Limit: limit},
		Skip:       skip,
	}
}

type baseCursor struct {
	// Done will be true if there is no more result after last fetch
	Done bool

	// Limit is maximum number of items retrieved from a request
	Limit int
}

// HasMore returns true if there is more document after the current batch.
// This value is meaning full only after UpdateFrom
func (c *baseCursor) HasMore() bool { return !c.Done }

func (c *baseCursor) updateFrom(res *ViewResponse) {
	lrows := len(res.Rows)
	if lrows <= c.Limit {
		c.Done = true
	} else {
		res.Rows = res.Rows[:lrows-1]
		c.Done = false
	}
}

// SkipCursor is a Cursor using Skip to know how deep in the request it is.
type SkipCursor struct {
	*baseCursor
	// Skip is the number of elements to start from
	Skip int
}

// ApplyTo applies the cursor to a ViewRequest
// the transformed ViewRequest will retrieve elements from Cursor to
// Limit or EndKey whichever comes first
// /!\ Mutates req
func (c *SkipCursor) ApplyTo(req *ViewRequest) {
	if c.Skip != 0 {
		req.Skip = c.Skip
	}
	if c.Limit != 0 {
		req.Limit = c.Limit + 1
	}
}

// UpdateFrom change the cursor status depending on information from
// the view's response
func (c *SkipCursor) UpdateFrom(res *ViewResponse) {
	c.baseCursor.updateFrom(res)
	c.Skip += c.Limit
}

// StartKeyCursor is a Cursor using start_key, ie a reference to the
// last fetched item to keep pagination
type StartKeyCursor struct {
	*baseCursor
	// NextKey & NextDocID contains a reference to the document
	// right after the last fetched one
	NextKey   interface{}
	NextDocID string
}

// ApplyTo applies the cursor to a ViewRequest
// the transformed ViewRequest will retrieve elements from Cursor to
// Limit or EndKey whichever comes first
// /!\ Mutates req
func (c *StartKeyCursor) ApplyTo(req *ViewRequest) {
	if c.NextKey != "" && c.NextKey != nil && c.keyInRange(req) {
		if req.Key != nil && req.StartKey == nil {
			req.StartKey = req.Key
			req.EndKey = req.Key
			req.InclusiveEnd = true
			req.Key = nil
		}

		req.StartKey = c.NextKey
		if c.NextDocID != "" {
			req.StartKeyDocID = c.NextDocID
		}
	}

	if c.Limit != 0 {
		req.Limit = c.Limit + 1
	}
}

// UpdateFrom change the cursor status depending on information from
// the view's response
func (c *StartKeyCursor) UpdateFrom(res *ViewResponse) {
	var next *ViewResponseRow
	lrows := len(res.Rows)
	if lrows > 0 {
		next = res.Rows[lrows-1]
	}
	c.baseCursor.updateFrom(res)
	if !c.Done {
		c.NextKey = next.Key
		c.NextDocID = next.ID
	} else {
		c.NextKey = nil
		c.NextDocID = ""
	}
}

// keyInRange tells if the cursor key keeps the part of the key that is fixed
// by the request (the prefix shared by its bounds). A key outside of it is
// ignored, so a cursor can only move inside the range chosen by the caller.
func (c *StartKeyCursor) keyInRange(req *ViewRequest) bool {
	var prefix []interface{}
	switch {
	case req.Key != nil:
		prefix = keyParts(req.Key)
	case req.StartKey != nil && req.EndKey != nil:
		start, end := keyParts(req.StartKey), keyParts(req.EndKey)
		for i := 0; i < len(start) && i < len(end) && reflect.DeepEqual(start[i], end[i]); i++ {
			prefix = append(prefix, start[i])
		}
	}
	next := keyParts(c.NextKey)
	return len(next) >= len(prefix) && reflect.DeepEqual(next[:len(prefix)], prefix)
}

func keyParts(key interface{}) []interface{} {
	var parts []interface{}
	if b, err := json.Marshal(key); err == nil && json.Unmarshal(b, &parts) == nil {
		return parts
	}
	return []interface{}{key}
}
