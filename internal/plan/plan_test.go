package plan

import (
	"errors"
	"testing"
	"time"
)

func TestNormalizeDefaults(t *testing.T) {
	in, err := Normalize(SaveInput{Name: "  Office start ", StartTime: "9:30", Weekdays: []int{5, 1, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if in.Name != "Office start" || in.StartTime != "09:30" || !*in.Notify {
		t.Fatalf("got %+v", in)
	}
	if len(in.Weekdays) != 2 || in.Weekdays[0] != 1 || in.Weekdays[1] != 5 {
		t.Fatalf("weekdays = %v", in.Weekdays)
	}

	def, _ := Normalize(SaveInput{Name: "Closing"})
	if def.StartTime != "09:00" || len(def.Weekdays) != 5 {
		t.Fatalf("defaults = %+v", def)
	}
}

func TestNormalizeRejects(t *testing.T) {
	for name, in := range map[string]SaveInput{
		"no name":   {},
		"bad time":  {Name: "a", StartTime: "25:00"},
		"bad day":   {Name: "a", Weekdays: []int{7}},
		"no days":   {Name: "a", Weekdays: []int{}},
		"bad color": {Name: "a", Color: "green"},
	} {
		if _, err := Normalize(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestNext(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	workdays := []int{1, 2, 3, 4, 5}
	// Monday 2026-10-12.
	mon := func(h, m int) time.Time { return time.Date(2026, 10, 12, h, m, 0, 0, ist) }

	cases := []struct {
		name  string
		days  []int
		after time.Time
		want  time.Time
	}{
		{"before start same day", workdays, mon(8, 0), mon(9, 0)},
		{"exactly at start moves on", workdays, mon(9, 0), mon(9, 0).AddDate(0, 0, 1)},
		{"friday evening skips weekend", workdays, mon(18, 0).AddDate(0, 0, 4), mon(9, 0).AddDate(0, 0, 7)},
		{"weekly on monday wraps a week", []int{1}, mon(10, 0), mon(9, 0).AddDate(0, 0, 7)},
		{"utc instant uses local day", []int{0}, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), time.Date(2026, 10, 18, 9, 0, 0, 0, ist)},
	}
	for _, c := range cases {
		got, ok := Next(c.days, "09:00", ist, c.after)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if _, ok := Next(nil, "09:00", ist, mon(8, 0)); ok {
		t.Error("no days should never occur")
	}
}

func TestOn(t *testing.T) {
	sat := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	if On([]int{1, 2, 3, 4, 5}, sat) || !On([]int{0, 6}, sat) {
		t.Fatal("weekday match wrong")
	}
}
