package api

import (
	"testing"
	"time"
)

func TestAdvanceRecurringMonthClampsToTargetMonth(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		months    int
		anchorDay int
		want      string
	}{
		{name: "normal February", current: "2025-01-31", months: 1, anchorDay: 31, want: "2025-02-28"},
		{name: "leap February", current: "2024-01-31", months: 1, anchorDay: 31, want: "2024-02-29"},
		{name: "returns to anchor", current: "2025-02-28", months: 1, anchorDay: 31, want: "2025-03-31"},
		{name: "quarterly clamp", current: "2025-11-30", months: 3, anchorDay: 31, want: "2026-02-28"},
		{name: "yearly leap clamp", current: "2024-02-29", months: 12, anchorDay: 29, want: "2025-02-28"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current, err := time.Parse("2006-01-02", tt.current)
			if err != nil {
				t.Fatal(err)
			}
			got := advanceRecurringMonth(current, tt.months, tt.anchorDay).Format("2006-01-02")
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
