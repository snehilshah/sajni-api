package api

import (
	"testing"
	"time"

	_ "time/tzdata" // embed zoneinfo so LoadLocation works on Windows/CI
)

func istLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load IST: %v", err)
	}
	return loc
}

// resolveTxnAt prefers the ISO txn_at, falls back to the legacy date-only field
// (read as IST midnight), then to now. Order and the IST midnight rule matter:
// they back-fill android / older web without shifting the day.
func TestResolveTxnAt(t *testing.T) {
	loc := istLoc(t)
	now := time.Date(2026, 6, 3, 9, 15, 0, 0, loc)

	// ISO wins, offset preserved as the same instant.
	got := resolveTxnAt(loc, "2026-06-02T14:30:00+05:30", "2026-01-01", now)
	if want := time.Date(2026, 6, 2, 14, 30, 0, 0, loc); !got.Equal(want) {
		t.Errorf("iso txn_at: got %v want %v", got, want)
	}

	// Legacy date-only → IST midnight (the "add 00" rule).
	got = resolveTxnAt(loc, "", "2026-06-02", now)
	if want := time.Date(2026, 6, 2, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("legacy date: got %v want %v", got, want)
	}

	// Nothing usable → now.
	if got = resolveTxnAt(loc, "", "", now); !got.Equal(now) {
		t.Errorf("empty: got %v want now %v", got, now)
	}

	// Garbage ISO falls through to legacy date, not now.
	got = resolveTxnAt(loc, "not-a-time", "2026-06-02", now)
	if want := time.Date(2026, 6, 2, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("bad iso → legacy: got %v want %v", got, want)
	}
}

// composeTxnAt merges an AI-parsed date + optional time; a blank time must fall
// back to the current minute (not midnight) so timeless messages still sort
// sensibly within their day.
func TestComposeTxnAt(t *testing.T) {
	loc := istLoc(t)
	now := time.Date(2026, 6, 3, 9, 15, 42, 0, loc)

	// Date + stated time.
	got := composeTxnAt(loc, "2026-06-02", "14:30", now)
	if want := time.Date(2026, 6, 2, 14, 30, 0, 0, loc); !got.Equal(want) {
		t.Errorf("date+time: got %v want %v", got, want)
	}

	// Date, no time → that date at the current clock (h:m:s of now).
	got = composeTxnAt(loc, "2026-06-02", "", now)
	if want := time.Date(2026, 6, 2, 9, 15, 42, 0, loc); !got.Equal(want) {
		t.Errorf("date only: got %v want %v", got, want)
	}

	// Bad date → now.
	if got = composeTxnAt(loc, "nope", "14:30", now); !got.Equal(now) {
		t.Errorf("bad date: got %v want now %v", got, now)
	}
}
