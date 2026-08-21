package reminder

import (
	"testing"
	"time"
)

func TestNextKeepsLocalClockAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.March, 7, 9, 30, 0, 0, loc)
	rule, err := Normalize(Rule{Frequency: "daily"}, start)
	if err != nil {
		t.Fatal(err)
	}
	next, ok := Next(start, start, 1, rule, loc)
	if !ok || next.Day() != 8 || next.Hour() != 9 || next.Minute() != 30 {
		t.Fatalf("next = %v, want Mar 8 at 09:30 local", next)
	}
}

func TestWeeklyCadenceUsesSelectedWeekdays(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, time.August, 17, 16, 0, 0, 0, loc) // Monday
	rule, err := Normalize(Rule{Frequency: "weekly", Interval: 2, Weekdays: []int{1, 4}}, start)
	if err != nil {
		t.Fatal(err)
	}
	next, ok := Next(start, start, 1, rule, loc)
	if !ok || next != time.Date(2026, time.August, 20, 16, 0, 0, 0, loc) {
		t.Fatalf("next = %v, want selected Thursday in the same active week", next)
	}
	next, ok = Next(start, next, 2, rule, loc)
	if !ok || next != time.Date(2026, time.August, 31, 16, 0, 0, 0, loc) {
		t.Fatalf("next = %v, want Monday in the next active fortnight", next)
	}
}

func TestMonthlyDateClampsAndOrdinalSkipsMissingMonth(t *testing.T) {
	loc := time.UTC
	jan31 := time.Date(2026, time.January, 31, 8, 0, 0, 0, loc)
	dateRule, err := Normalize(Rule{Frequency: "monthly", MonthDay: 31}, jan31)
	if err != nil {
		t.Fatal(err)
	}
	next, ok := Next(jan31, jan31, 1, dateRule, loc)
	if !ok || next != time.Date(2026, time.February, 28, 8, 0, 0, 0, loc) {
		t.Fatalf("next = %v, want Feb 28", next)
	}

	fifthMonday := time.Date(2026, time.June, 29, 8, 0, 0, 0, loc)
	weekdayRule, err := Normalize(Rule{Frequency: "monthly", MonthlyMode: "weekday", Weekday: 1, Ordinal: 5}, fifthMonday)
	if err != nil {
		t.Fatal(err)
	}
	next, ok = Next(fifthMonday, fifthMonday, 1, weekdayRule, loc)
	if !ok || next != time.Date(2026, time.August, 31, 8, 0, 0, 0, loc) {
		t.Fatalf("next = %v, want next month that has a fifth Monday", next)
	}
}

func TestRecurrenceStopsAtCountAndUntil(t *testing.T) {
	start := time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC)
	countRule, _ := Normalize(Rule{Frequency: "daily", Count: 2}, start)
	if _, ok := Next(start, start.AddDate(0, 0, 1), 2, countRule, time.UTC); ok {
		t.Fatal("count-limited recurrence returned an extra occurrence")
	}
	untilRule, _ := Normalize(Rule{Frequency: "daily", Until: "2026-08-21"}, start)
	if _, ok := Next(start, start.AddDate(0, 0, 1), 2, untilRule, time.UTC); ok {
		t.Fatal("until-limited recurrence returned an occurrence after the end date")
	}
}
