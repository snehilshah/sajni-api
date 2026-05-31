package api

import (
	"strings"
	"testing"
	"time"

	// Embed the zoneinfo DB so time.LoadLocation works in tests too — the
	// dev box / CI (Windows) has no system zoneinfo, same as the distroless
	// Cloud Run image where main.go provides this import.
	_ "time/tzdata"
)

// defaultLoc must resolve to IST (+05:30), not UTC — every Sajni user is IST,
// so an unset timezone falling back to UTC would shift reminder clock times.
func TestDefaultLocIsIST(t *testing.T) {
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, offset := at.In(defaultLoc).Zone(); offset != 5*3600+30*60 {
		t.Fatalf("defaultLoc offset = %ds, want 19800 (+05:30)", offset)
	}
}

// 11:30 UTC is 17:00 IST; formatReminderWhen must render the user's clock.
func TestFormatReminderWhenRendersUserClock(t *testing.T) {
	at := time.Date(2026, 6, 1, 11, 30, 0, 0, time.UTC)
	if got := formatReminderWhen(at, "Asia/Kolkata"); !strings.Contains(got, "5:00 PM") {
		t.Fatalf("formatReminderWhen(IST) = %q, want it to contain %q", got, "5:00 PM")
	}
}

// Empty/unknown tz must fall back to defaultLoc (IST), NOT UTC — this is the
// regression the backfill + fallback fix guards against.
func TestFormatReminderWhenFallsBackToIST(t *testing.T) {
	at := time.Date(2026, 6, 1, 11, 30, 0, 0, time.UTC) // 17:00 IST / 11:30 UTC
	got := formatReminderWhen(at, "")
	if strings.Contains(got, "11:30 AM") {
		t.Fatalf("empty tz fell back to UTC (%q); expected IST", got)
	}
	if !strings.Contains(got, "5:00 PM") {
		t.Fatalf("empty tz = %q, want IST clock containing %q", got, "5:00 PM")
	}
}

func TestSameDay(t *testing.T) {
	a := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	b := time.Date(2026, 6, 1, 23, 0, 0, 0, time.UTC)
	c := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	if !sameDay(a, b) {
		t.Error("sameDay(a,b) = false, want true (same calendar day)")
	}
	if sameDay(b, c) {
		t.Error("sameDay(b,c) = true, want false (different days)")
	}
}
