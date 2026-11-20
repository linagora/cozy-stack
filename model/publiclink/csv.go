package publiclink

import (
	"context"
	"encoding/csv"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// WriteCSV writes the retained events in chronological order.
func WriteCSV(ctx context.Context, out io.Writer, db prefixer.Prefixer, linkID string) error {
	return writeCSV(out, func(visit func(Event) error) error {
		return ForEachEvent(ctx, db, linkID, visit)
	})
}

// WriteReportCSV writes the retained events through expiry, inclusive, using
// the same format as the interactive export. The caller authorizes the report.
func WriteReportCSV(ctx context.Context, out io.Writer, db prefixer.Prefixer, linkID string, expiry time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := couchdb.DBStatus(db, consts.PublicLinkEvents); couchdb.IsNoDatabaseError(err) {
		if err := couchdb.DefineView(db, couchdb.PublicLinkEventsByTime); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return writeCSV(out, func(visit func(Event) error) error {
		return ForEachEvent(ctx, db, linkID, func(event Event) error {
			if event.OccurredAt.After(expiry) {
				return nil
			}
			return visit(event)
		})
	})
}

func writeCSV(out io.Writer, iterate func(func(Event) error) error) error {
	writer := csv.NewWriter(out)
	if err := writer.Write([]string{"occurred_at", "IP", "identity", "action"}); err != nil {
		return err
	}
	err := iterate(func(event Event) error {
		row := []string{event.OccurredAt.UTC().Format(time.RFC3339Nano), event.IP, event.Identity, string(event.Action)}
		for i, value := range row {
			row[i] = safeCSVCell(value)
		}
		return writer.Write(row)
	})
	if err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}

func safeCSVCell(value string) string {
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") ||
		(len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0]))) {
		return "'" + value
	}
	return value
}
