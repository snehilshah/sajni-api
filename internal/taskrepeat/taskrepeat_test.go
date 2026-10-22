package taskrepeat

import (
	"errors"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

func TestNormalizeScope(t *testing.T) {
	if _, err := Normalize(Rule{Frequency: "daily"}, Scope{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("undated: err = %v", err)
	}
	if _, err := Normalize(Rule{Frequency: "daily"}, Scope{Week: "2026-10-19"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("daily week task: err = %v", err)
	}
	r, err := Normalize(Rule{Frequency: "weekly", Weekdays: []int{4, 1}, Count: 3, Until: "2027-01-01"}, Scope{Due: "2026-10-19"})
	if err != nil || r.Interval != 1 || len(r.Weekdays) != 2 || r.Weekdays[0] != 1 || r.Count != 0 || r.Until != "" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if r, _ := Normalize(Rule{}, Scope{}); r.Recurring() {
		t.Fatal("empty rule should not repeat")
	}
	if Encode(Rule{}) != "{}" || Decode([]byte("{}")).Recurring() {
		t.Fatal("empty rule encoding")
	}
}

func TestNext(t *testing.T) {
	workdays := Rule{Frequency: "weekly", Interval: 1, Weekdays: []int{1, 2, 3, 4, 5}}
	cases := []struct {
		name            string
		rule            Rule
		kind            string
		from, notBefore string
		want            string
	}{
		{"daily next day", Rule{Frequency: "daily", Interval: 1}, "day", "2026-10-19", "2026-10-20", "2026-10-20"},
		{"daily catches up to today", Rule{Frequency: "daily", Interval: 1}, "day", "2026-10-15", "2026-10-20", "2026-10-20"},
		{"workdays skip weekend", workdays, "day", "2026-10-23", "2026-10-24", "2026-10-26"},
		{"every 2 days", Rule{Frequency: "daily", Interval: 2}, "day", "2026-10-19", "2026-10-20", "2026-10-21"},
		{"monthly on the 31st clamps", Rule{Frequency: "monthly", Interval: 1, MonthlyMode: "date", MonthDay: 31}, "day", "2026-01-31", "2026-02-01", "2026-02-28"},
		{"week task", Rule{Frequency: "weekly", Interval: 1}, "week", "2026-10-12", "2026-10-19", "2026-10-19"},
		{"fortnightly week task", Rule{Frequency: "weekly", Interval: 2}, "week", "2026-10-12", "2026-10-19", "2026-10-26"},
		{"month task", Rule{Frequency: "monthly", Interval: 1}, "month", "2026-09-01", "2026-10-01", "2026-10-01"},
	}
	for _, c := range cases {
		got, ok := Next(c.rule, c.kind, day(c.from), day(c.notBefore))
		if !ok || got.Format("2006-01-02") != c.want {
			t.Errorf("%s: got %v %v, want %s", c.name, got.Format("2006-01-02"), ok, c.want)
		}
	}
	if _, ok := Next(Rule{}, "day", day("2026-10-19"), day("2026-10-20")); ok {
		t.Error("a one-off task has no next date")
	}
}

func TestUntick(t *testing.T) {
	got := untick([]byte(`[{"id":"a","text":"Check mail","done":true},{"id":"b","text":"Standup","done":false}]`))
	if got != `[{"done":false,"id":"a","text":"Check mail"},{"done":false,"id":"b","text":"Standup"}]` {
		t.Fatalf("got %s", got)
	}
	if untick(nil) != "[]" {
		t.Fatal("nil steps")
	}
}
