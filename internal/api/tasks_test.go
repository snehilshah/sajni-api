package api

import (
	"testing"
	"time"
)

func TestTaskColorValidation(t *testing.T) {
	for _, color := range []string{"#2D5A4F", "#7c9a92", " #C49A6C ", "#A14B4F", "#4F6FA1", "#8B6FA1", "#7A7A7A"} {
		if !validTaskColor(color) {
			t.Errorf("validTaskColor(%q) = false", color)
		}
	}
	for _, color := range []string{"", "red", "#FFFFFF", "#2D5A4F00"} {
		if validTaskColor(color) {
			t.Errorf("validTaskColor(%q) = true", color)
		}
	}
}

func TestPlannerCalendarHelpersAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 7, 0, 0, 0, 0, loc)
	to := time.Date(2026, 3, 9, 0, 0, 0, 0, loc)
	if got := calendarDays(from, to); got != 2 {
		t.Fatalf("calendarDays across DST = %d, want 2", got)
	}
	original := time.Date(2026, 3, 7, 9, 30, 0, 0, loc)
	shifted := shiftCalendarDays(original, 2, loc)
	if shifted.Hour() != 9 || shifted.Minute() != 30 || shifted.Format("2006-01-02") != "2026-03-09" {
		t.Fatalf("shiftCalendarDays = %s, want 2026-03-09 09:30 local", shifted)
	}
	if got := mondayAt(time.Date(2026, 8, 26, 14, 0, 0, 0, loc)).Format("2006-01-02"); got != "2026-08-24" {
		t.Fatalf("mondayAt = %s, want 2026-08-24", got)
	}
}

func TestValidTaskStatus(t *testing.T) {
	for _, status := range []string{"todo", "in_progress", "blocked", "done", "scratched"} {
		if !validTaskStatus(status) {
			t.Errorf("validTaskStatus(%q) = false", status)
		}
	}
	for _, status := range []string{"", "missed", "cancelled", "BLOCKED"} {
		if validTaskStatus(status) {
			t.Errorf("validTaskStatus(%q) = true", status)
		}
	}
}

// rescheduleOutcome is the single source of truth for "did the user salvage a
// lapsed task, or did the day stand as a miss?" — it drives both the task
// lifecycle rows and what the Missed banner stops counting. today is the
// user's local YYYY-MM-DD.
func TestRescheduleOutcome(t *testing.T) {
	const today = "2026-06-05"
	cases := []struct {
		name        string
		oldDate     string
		newDate     string
		wantOutcome string
		wantResched bool
	}{
		{"past day moved forward = rescheduled", "2026-06-03", "2026-06-10", "rescheduled", true},
		{"past day moved to today = rescheduled", "2026-06-03", today, "rescheduled", true},
		{"today is not yet missed", today, "2026-06-10", "missed", false},
		{"future day is not a miss", "2026-06-09", "2026-06-12", "missed", false},
		{"clearing the date (empty new) = missed", "2026-06-03", "", "missed", false},
		{"whitespace new date = missed", "2026-06-03", "   ", "missed", false},
		{"no prior date = missed", "", "2026-06-10", "missed", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotOutcome, gotResched := rescheduleOutcome(c.oldDate, c.newDate, today)
			if gotOutcome != c.wantOutcome || gotResched != c.wantResched {
				t.Fatalf("rescheduleOutcome(%q,%q,%q) = (%q,%v), want (%q,%v)",
					c.oldDate, c.newDate, today, gotOutcome, gotResched, c.wantOutcome, c.wantResched)
			}
		})
	}
}

// normalizeSteps must trim text, drop empties, mint a stable id for any step
// missing one, and preserve ids that are already set.
func TestNormalizeSteps(t *testing.T) {
	in := []Step{
		{ID: "keep-me", Text: "  buy milk  ", Done: true}, // trimmed, id kept
		{ID: "", Text: "call mom"},                        // gets an id
		{ID: "", Text: "   "},                             // dropped (blank)
		{ID: "", Text: ""},                                // dropped (empty)
	}
	out := normalizeSteps(in)
	if len(out) != 2 {
		t.Fatalf("normalizeSteps len = %d, want 2 (empties dropped); got %+v", len(out), out)
	}
	if out[0].Text != "buy milk" {
		t.Errorf("step[0].Text = %q, want trimmed %q", out[0].Text, "buy milk")
	}
	if !out[0].Done || out[0].ID != "keep-me" {
		t.Errorf("step[0] lost its fields: %+v", out[0])
	}
	if out[1].ID == "" {
		t.Errorf("step[1] missing minted id: %+v", out[1])
	}
}

// decodeSteps must never panic on bad input — empty and malformed JSONB both
// degrade to an empty (non-nil) slice; valid JSON round-trips.
func TestDecodeSteps(t *testing.T) {
	if got := decodeSteps(nil); got == nil || len(got) != 0 {
		t.Errorf("decodeSteps(nil) = %+v, want empty non-nil slice", got)
	}
	if got := decodeSteps([]byte("not json")); got == nil || len(got) != 0 {
		t.Errorf("decodeSteps(malformed) = %+v, want empty non-nil slice", got)
	}
	got := decodeSteps([]byte(`[{"id":"s1","text":"do it","done":true}]`))
	if len(got) != 1 || got[0].ID != "s1" || got[0].Text != "do it" || !got[0].Done {
		t.Errorf("decodeSteps(valid) = %+v, want one parsed step", got)
	}
}
