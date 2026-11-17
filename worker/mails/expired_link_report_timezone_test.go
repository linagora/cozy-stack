package mails

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReportMailTimezone(t *testing.T) {
	for _, tt := range []struct {
		name       string
		preference interface{}
		instant    string
		want       string
	}{
		{"Paris winter", "Europe/Paris", "2026-01-15T23:30:00Z", "2026-01-16 00:30 CET +01:00"},
		{"Paris summer", "Europe/Paris", "2026-07-15T23:30:00Z", "2026-07-16 01:30 CEST +02:00"},
		{"spring before", "Europe/Paris", "2026-03-29T00:59:00Z", "2026-03-29 01:59 CET +01:00"},
		{"spring after", "Europe/Paris", "2026-03-29T01:00:00Z", "2026-03-29 03:00 CEST +02:00"},
		{"autumn first hour", "Europe/Paris", "2026-10-25T00:30:00Z", "2026-10-25 02:30 CEST +02:00"},
		{"autumn repeated hour", "Europe/Paris", "2026-10-25T01:30:00Z", "2026-10-25 02:30 CET +01:00"},
		{"non-European fractional zone", "Asia/Kolkata", "2026-07-15T20:00:00Z", "2026-07-16 01:30 IST +05:30"},
		{"absent", nil, "2026-07-15T20:00:00Z", "2026-07-15 20:00 UTC +00:00"},
		{"empty", "", "2026-07-15T20:00:00Z", "2026-07-15 20:00 UTC +00:00"},
		{"wrong type", 42, "2026-07-15T20:00:00Z", "2026-07-15 20:00 UTC +00:00"},
		{"invalid", "Not/AZone", "2026-07-15T20:00:00Z", "2026-07-15 20:00 UTC +00:00"},
		{"server local is not a preference", "Local", "2026-07-15T20:00:00Z", "2026-07-15 20:00 UTC +00:00"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tt.instant)
			require.NoError(t, err)
			location := reportMailLocation(tt.preference)
			require.Equal(t, tt.want, formatReportMailTime(at, location))
			rows := [][]string{{tt.instant, "192.0.2.1", "", "opened"}}
			got := formatReportPreview(rows, location)
			require.Equal(t, tt.want, got[0][0])
			require.Empty(t, got[0][2], "identity is translated by the template, not stored in the CSV")
			require.Equal(t, tt.instant, at.Format(time.RFC3339), "formatting must not alter the instant")
		})
	}
}

func TestReportMailPreviewIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, identity, want string
	}{
		{"absent", "", ""},
		{"whitespace", " \t ", ""},
		{"CSV protected tab", "'\t ", ""},
		{"CSV protected CR", "'\r ", ""},
		{"CSV protected newline", "'\n ", ""},
		{"normal identity", "Alice <alice@example.test>", "Alice <alice@example.test>"},
		{"apostrophe identity", "'Alice", "'Alice"},
		{"apostrophe and spaces", "'  ", "'  "},
		{"protected meaningful identity", "'\t Alice", "'\t Alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := [][]string{{"2026-10-25T00:30:00Z", "192.0.2.1", tc.identity, "opened"}}
			got := formatReportPreview(rows, reportMailLocation("Europe/Paris"))
			require.Equal(t, []string{"2026-10-25 02:30 CEST +02:00", "192.0.2.1", tc.want, "opened"}, got[0])
		})
	}
	rows := [][]string{{"invalid timestamp", "192.0.2.1", " \t ", "opened"}}
	require.Equal(t, []string{"invalid timestamp", "192.0.2.1", "", "opened"}, formatReportPreview(rows, time.UTC)[0])
}
