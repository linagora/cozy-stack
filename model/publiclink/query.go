package publiclink

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// ForEachEvent visits one link's retained events in chronological order.
func ForEachEvent(ctx context.Context, db prefixer.Prefixer, linkID string, visit func(Event) error) error {
	const pageSize = 500
	req := &couchdb.ViewRequest{
		StartKey: []interface{}{linkID}, EndKey: []interface{}{linkID, map[string]interface{}{}},
		IncludeDocs: true,
	}
	cursor := couchdb.NewKeyCursor(pageSize, nil, "")
	for cursor.HasMore() {
		cursor.ApplyTo(req)
		if err := ctx.Err(); err != nil {
			return err
		}
		var result couchdb.ViewResponse
		if err := couchdb.ExecView(db, couchdb.PublicLinkEventsByTime, req, &result); err != nil {
			return err
		}
		count := len(result.Rows)
		if count > pageSize {
			count = pageSize
		}
		for _, row := range result.Rows[:count] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(row.Doc) == 0 || string(row.Doc) == "null" {
				return errors.New("missing public link event document")
			}
			var event Event
			if err := json.Unmarshal(row.Doc, &event); err != nil {
				return err
			}
			if event.ID() != row.ID || event.LinkID != linkID {
				return errors.New("public link event changed during export")
			}
			if err := visit(event); err != nil {
				return err
			}
		}
		if len(result.Rows) > pageSize {
			next := result.Rows[pageSize]
			if next.ID == "" || next.ID == req.StartKeyDocID {
				return errors.New("invalid public link events cursor")
			}
		}
		cursor.UpdateFrom(&result)
	}
	return nil
}
