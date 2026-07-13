package api

import "time"

// advanceRecurringMonth preserves the schedule's original day where possible
// and clamps it to the target month's final day otherwise.
func advanceRecurringMonth(current time.Time, months, anchorDay int) time.Time {
	if anchorDay < 1 || anchorDay > 31 {
		anchorDay = current.Day()
	}
	target := time.Date(current.Year(), current.Month()+time.Month(months), 1, 0, 0, 0, 0, current.Location())
	lastDay := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, target.Location()).Day()
	if anchorDay > lastDay {
		anchorDay = lastDay
	}
	return time.Date(target.Year(), target.Month(), anchorDay, current.Hour(), current.Minute(), current.Second(), current.Nanosecond(), current.Location())
}
