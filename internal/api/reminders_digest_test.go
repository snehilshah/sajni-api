package api

import (
	"testing"
	"time"

	_ "time/tzdata"
)

// isLastDayOfMonth must hold only on the final calendar day, across short
// months, leap Februaries, and December (year rollover).
func TestIsLastDayOfMonth(t *testing.T) {
	cases := []struct {
		y, m, d int
		want    bool
	}{
		{2026, 6, 30, true},  // June has 30 days
		{2026, 6, 29, false}, // day before
		{2026, 2, 28, true},  // non-leap February
		{2024, 2, 29, true},  // leap February
		{2024, 2, 28, false}, // not last in a leap year
		{2026, 12, 31, true}, // year boundary
		{2026, 1, 31, true},  // 31-day month
	}
	for _, c := range cases {
		at := time.Date(c.y, time.Month(c.m), c.d, 10, 0, 0, 0, defaultLoc)
		if got := isLastDayOfMonth(at); got != c.want {
			t.Errorf("isLastDayOfMonth(%04d-%02d-%02d) = %v, want %v", c.y, c.m, c.d, got, c.want)
		}
	}
}

// The weekly digest only fires on Fridays; mondayOf must anchor to that week's
// Monday so the week_of <= anchor predicate also catches carried-over tasks.
func TestWeeklyDigestAnchor(t *testing.T) {
	// 2026-06-19 is a Friday; its ISO week starts Mon 2026-06-15.
	fri := time.Date(2026, 6, 19, 10, 0, 0, 0, defaultLoc)
	if fri.Weekday() != time.Friday {
		t.Fatalf("fixture not a Friday: %v", fri.Weekday())
	}
	if got := mondayOf(fri).Format("2006-01-02"); got != "2026-06-15" {
		t.Errorf("mondayOf(Fri 2026-06-19) = %s, want 2026-06-15", got)
	}
}
