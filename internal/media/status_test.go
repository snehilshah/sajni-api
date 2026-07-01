package media

import "testing"

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
