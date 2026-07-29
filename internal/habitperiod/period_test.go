package habitperiod

import (
	"testing"
	"time"
)

func mustDate(t *testing.T, value string) time.Time {
	t.Helper()
	d, err := ParseDate(value)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFortnightCalendarSequence(t *testing.T) {
	tests := map[string]string{
		"2026-07-01": "2026-06-29",
		"2026-07-12": "2026-06-29",
		"2026-07-13": "2026-07-13",
		"2026-08-02": "2026-07-27",
		"2026-08-03": "2026-07-27",
		"2026-08-10": "2026-08-10",
		"2027-01-01": "2026-12-28",
	}
	for input, want := range tests {
		got := ForDate(mustDate(t, input), Fortnightly).Start.Format("2006-01-02")
		if got != want {
			t.Errorf("ForDate(%s) = %s, want %s", input, got, want)
		}
	}
}

func TestStreaksGroupLegacyDatesByCadence(t *testing.T) {
	logs := []string{
		"2026-06-30", "2026-07-04",
		"2026-07-14",
		"2026-07-28", "2026-08-03",
	}
	current, longest := Streaks(logs, Fortnightly, mustDate(t, "2026-08-05"))
	if current != 3 || longest != 3 {
		t.Fatalf("Streaks() = %d, %d; want 3, 3", current, longest)
	}
}

func TestOpenCurrentPeriodDoesNotBreakStreak(t *testing.T) {
	logs := []string{"2026-06-02", "2026-06-09", "2026-06-16"}
	current, longest := Streaks(logs, Weekly, mustDate(t, "2026-06-24"))
	if current != 3 || longest != 3 {
		t.Fatalf("Streaks() = %d, %d; want 3, 3", current, longest)
	}
}
