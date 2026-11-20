package publiclink

import "time"

// ReportWindow returns the current or next UTC window for starting report work.
func ReportWindow(now time.Time) (start, end time.Time) {
	year, month, day := now.UTC().Date()
	start = time.Date(year, month, day, 2, 0, 0, 0, time.UTC)
	end = start.Add(4 * time.Hour)
	if !now.Before(end) {
		start = start.AddDate(0, 0, 1)
		end = end.AddDate(0, 0, 1)
	}
	return start, end
}

// ReportWindowOpen reports whether now is inside the half-open window.
func ReportWindowOpen(now time.Time) bool {
	start, _ := ReportWindow(now)
	return !now.Before(start)
}
