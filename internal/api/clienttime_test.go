package api

import "testing"

func TestParseClientTime(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-23T09:45:00+05:30":       "2026-11-23T04:15:00Z",
		"2026-11-23T09:45+05:30":          "2026-11-23T04:15:00Z",
		" 2026-11-23T09:45:12.345+05:30 ": "2026-11-23T04:15:12Z",
		"2026-11-23T04:15Z":               "2026-11-23T04:15:00Z",
	} {
		got, err := parseClientTime(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if s := got.UTC().Truncate(1e9).Format("2006-01-02T15:04:05Z07:00"); s != want {
			t.Errorf("%q: got %s, want %s", in, s, want)
		}
	}
	for _, bad := range []string{"", "2026-11-23", "09:45", "2026-11-23T09:45"} {
		if _, err := parseClientTime(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
