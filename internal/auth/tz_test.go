package auth

import "testing"

// canonicalTZ collapses the deprecated Asia/Calcutta alias to Asia/Kolkata so
// the users.timezone column stays consistent; everything else passes through.
func TestCanonicalTZ(t *testing.T) {
	cases := map[string]string{
		"Asia/Calcutta":    "Asia/Kolkata",
		"Asia/Kolkata":     "Asia/Kolkata",
		"America/New_York": "America/New_York",
		"":                 "",
	}
	for in, want := range cases {
		if got := canonicalTZ(in); got != want {
			t.Errorf("canonicalTZ(%q) = %q, want %q", in, got, want)
		}
	}
}
