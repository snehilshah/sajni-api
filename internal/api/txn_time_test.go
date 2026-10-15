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

// resolveTxnAt parses the ISO txn_at and falls back to now — the legacy
// txn_date shim is gone now that both clients send txn_at.
func TestResolveTxnAt(t *testing.T) {
	loc := istLoc(t)
	now := time.Date(2026, 6, 3, 9, 15, 0, 0, loc)

	// ISO parses, offset preserved as the same instant.
	got := resolveTxnAt("2026-06-02T14:30:00+05:30", now)
	if want := time.Date(2026, 6, 2, 14, 30, 0, 0, loc); !got.Equal(want) {
		t.Errorf("iso txn_at: got %v want %v", got, want)
	}

	// Empty → now.
	if got = resolveTxnAt("", now); !got.Equal(now) {
		t.Errorf("empty: got %v want now %v", got, now)
	}

	// Garbage → now.
	if got = resolveTxnAt("not-a-time", now); !got.Equal(now) {
		t.Errorf("bad iso: got %v want now %v", got, now)
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

// resolveTxnAt keeps a bare date's day (older clients' "Record received" sent
// only a date, which used to be dropped for now).
func TestResolveTxnAtBareDate(t *testing.T) {
	loc := istLoc(t)
	now := time.Date(2026, 6, 3, 9, 15, 42, 0, loc)
	if got, want := resolveTxnAt("2026-06-01", now), time.Date(2026, 6, 1, 9, 15, 42, 0, loc); !got.Equal(want) {
		t.Errorf("bare date: got %v want %v", got, want)
	}
	if got, want := resolveTxnAt("2026-06-01T18:05:00+05:30", now), time.Date(2026, 6, 1, 18, 5, 0, 0, loc); !got.Equal(want) {
		t.Errorf("rfc3339: got %v want %v", got, want)
	}
	if got := resolveTxnAt("", now); !got.Equal(now) {
		t.Errorf("blank: got %v want now", got)
	}
	if got := resolveTxnAt("junk", now); !got.Equal(now) {
		t.Errorf("junk: got %v want now", got)
	}
}
