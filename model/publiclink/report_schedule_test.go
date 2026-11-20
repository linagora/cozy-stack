package publiclink

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReportWindow(t *testing.T) {
	start := time.Date(2025, time.March, 30, 2, 0, 0, 0, time.UTC)
	end := time.Date(2025, time.March, 30, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		open bool
		next int
	}{
		{"restart before window", start.Add(-time.Nanosecond), false, 0},
		{"exact start", start, true, 0},
		{"restart during window in another zone", start.Add(time.Minute).In(time.FixedZone("UTC+03:00", 3*3600)), true, 0},
		{"last instant", end.Add(-time.Nanosecond), true, 0},
		{"exact close", end, false, 1},
		{"restart during daytime", end.Add(8 * time.Hour), false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wStart, wEnd := ReportWindow(tc.at)
			require.Equal(t, start.AddDate(0, 0, tc.next), wStart)
			require.Equal(t, end.AddDate(0, 0, tc.next), wEnd)
			require.Equal(t, tc.open, ReportWindowOpen(tc.at))
		})
	}
}
