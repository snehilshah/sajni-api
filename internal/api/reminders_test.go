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

// defaultLoc must resolve to IST (+05:30), not UTC, so older accounts without
// a captured timezone retain the product's historical clock.
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

func TestScheduledNotificationWindowUsesUserClock(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		tz   string
		want bool
	}{
		{
			name: "ten in Kolkata",
			at:   time.Date(2026, 7, 27, 4, 30, 0, 0, time.UTC),
			tz:   "Asia/Kolkata",
			want: true,
		},
		{
			name: "ten in New York during daylight time",
			at:   time.Date(2026, 7, 27, 14, 0, 0, 0, time.UTC),
			tz:   "America/New_York",
			want: true,
		},
		{
			name: "ten in Kathmandu quarter-hour zone",
			at:   time.Date(2026, 7, 27, 4, 15, 0, 0, time.UTC),
			tz:   "Asia/Kathmandu",
			want: true,
		},
		{
			name: "outside first quarter hour",
			at:   time.Date(2026, 7, 27, 4, 45, 0, 0, time.UTC),
			tz:   "Asia/Kolkata",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localNow := tt.at.In(timezoneLocation(tt.tz))
			if got := scheduledNotificationWindow(localNow); got != tt.want {
				t.Fatalf("scheduledNotificationWindow(%v in %s) = %v, want %v", tt.at, tt.tz, got, tt.want)
			}
		})
	}
}

func TestMediaReleaseReminderDateUsesLocalCalendar(t *testing.T) {
	at := time.Date(2026, 7, 27, 18, 45, 0, 0, time.UTC)
	if got := mediaReleaseReminderDate(at.In(timezoneLocation("Asia/Kolkata"))); got != "2026-07-29" {
		t.Fatalf("Kolkata reminder target = %s, want 2026-07-29", got)
	}
	if got := mediaReleaseReminderDate(at.In(timezoneLocation("America/New_York"))); got != "2026-07-28" {
		t.Fatalf("New York reminder target = %s, want 2026-07-28", got)
	}
}
