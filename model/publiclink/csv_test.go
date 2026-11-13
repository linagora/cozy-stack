package publiclink

import (
	"bytes"
	"encoding/csv"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriteCSV(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	events := []Event{
		{OccurredAt: at, Action: Created, IP: "192.0.2.1"},
		{OccurredAt: at.Add(time.Hour), Action: Opened, IP: "2001:db8::1", Identity: "  =HYPERLINK(\"bad\")"},
		{OccurredAt: at.Add(2 * time.Hour), Action: Downloaded, IP: "\tbad", Identity: "alice, \"example\""},
	}
	var buf bytes.Buffer
	require.NoError(t, writeCSV(&buf, func(visit func(Event) error) error {
		for _, event := range events {
			if err := visit(event); err != nil {
				return err
			}
		}
		return nil
	}))
	rows, err := csv.NewReader(&buf).ReadAll()
	require.NoError(t, err)
	require.Equal(t, [][]string{
		{"occurred_at", "IP", "identity", "action"},
		{"2026-01-02T03:04:05Z", "192.0.2.1", "", "created"},
		{"2026-01-02T04:04:05Z", "2001:db8::1", "'  =HYPERLINK(\"bad\")", "opened"},
		{"2026-01-02T05:04:05Z", "'\tbad", "alice, \"example\"", "downloaded"},
	}, rows)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestWriteCSVError(t *testing.T) {
	require.Error(t, writeCSV(failingWriter{}, func(func(Event) error) error { return nil }))
}
