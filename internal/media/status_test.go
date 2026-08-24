package media

import (
	"testing"
	"time"
)

func TestNormalizeStatus(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Status
		ok   bool
	}{
		{name: "empty defaults pending", raw: "", want: StatusPending, ok: true},
		{name: "done alias becomes complete", raw: "done", want: StatusComplete, ok: true},
		{name: "completed alias becomes complete", raw: "completed", want: StatusComplete, ok: true},
		{name: "watching alias becomes in progress", raw: "watching", want: StatusInProgress, ok: true},
		{name: "new season alias becomes system status", raw: "new season", want: StatusNewSeason, ok: true},
		{name: "canonical upcoming stays upcoming", raw: "upcoming", want: StatusUpcoming, ok: true},
		{name: "reject invalid", raw: "finished_pending", want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeStatus(tt.raw)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveStatus(t *testing.T) {
	today := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		kind        string
		requested   Status
		releaseDate string
		want        Status
	}{
		{name: "future movie is upcoming", kind: "movie", requested: StatusPending, releaseDate: "2026-08-25", want: StatusUpcoming},
		{name: "future show is upcoming", kind: "show", requested: StatusComplete, releaseDate: "2026-09-01", want: StatusUpcoming},
		{name: "release day is pending", kind: "movie", requested: StatusUpcoming, releaseDate: "2026-08-24", want: StatusPending},
		{name: "past release keeps user status", kind: "show", requested: StatusComplete, releaseDate: "2026-08-23", want: StatusComplete},
		{name: "dateless upcoming is pending", kind: "movie", requested: StatusUpcoming, want: StatusPending},
		{name: "books cannot be upcoming", kind: "book", requested: StatusUpcoming, releaseDate: "2026-09-01", want: StatusPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveStatus(tt.kind, tt.requested, tt.releaseDate, today); got != tt.want {
				t.Fatalf("ResolveStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}
