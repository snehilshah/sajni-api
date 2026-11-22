package api

import (
	"strings"
	"time"
)

// parseClientTime reads a timestamp a client sent: RFC3339, or the same
// without seconds ("2026-11-23T09:45+05:30"), which is what Java's
// OffsetDateTime.toString() prints when the seconds are zero (Android
// before 1.48.3 sent event times that way).
func parseClientTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	t, err := time.Parse(time.RFC3339, s)
	if err == nil {
		return t, nil
	}
	if m, merr := time.Parse("2006-01-02T15:04Z07:00", s); merr == nil {
		return m, nil
	}
	return time.Time{}, err
}
