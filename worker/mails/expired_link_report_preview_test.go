package mails

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatReportPreview(t *testing.T) {
	rows := [][]string{
		{"2026-10-28T18:02:30Z", "192.0.2.1", "Alice", "updated"},
		{"2026-10-29T11:25:00+02:00", "192.0.2.2", "Bob", "opened"},
		{"2026-10-29T14:10:00.123Z", "192.0.2.3", "Carol", "downloaded"},
	}
	require.Equal(t, [][]string{
		{"2026-10-29 14:10 UTC", "192.0.2.3", "Carol", "downloaded"},
		{"2026-10-29 09:25 UTC", "192.0.2.2", "Bob", "opened"},
		{"2026-10-28 18:02 UTC", "192.0.2.1", "Alice", "updated"},
	}, formatReportPreview(rows))
	require.Empty(t, formatReportPreview(nil))
}
