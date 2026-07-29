package habitperiod

import (
	"fmt"
	"sort"
	"time"
)

const (
	Daily       = "daily"
	Weekly      = "weekly"
	Fortnightly = "fortnightly"
	Monthly     = "monthly"
)

// The shared fortnight rhythm starts on this Monday and repeats every 14 days.
// It yields Jul 2026 W1/W3/W5, followed by Aug W2/W4, and continues cleanly
// across month and year boundaries.
var fortnightEpoch = time.Date(1970, time.January, 12, 0, 0, 0, 0, time.UTC)

type Period struct {
	Start time.Time
	End   time.Time
}

func ValidFrequency(frequency string) bool {
	switch frequency {
	case Daily, Weekly, Fortnightly, Monthly:
		return true
	default:
		return false
	}
}

func ParseDate(value string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q", value)
	}
	return d, nil
}

func ForDate(date time.Time, frequency string) Period {
	date = day(date)
	var start, next time.Time

	switch frequency {
	case Weekly:
		start = weekStart(date)
		next = start.AddDate(0, 0, 7)
	case Fortnightly:
		monday := weekStart(date)
		weeks := int(monday.Sub(fortnightEpoch).Hours()/24) / 7
		if weeks%2 != 0 {
			monday = monday.AddDate(0, 0, -7)
		}
		start = monday
		next = start.AddDate(0, 0, 14)
	case Monthly:
		start = time.Date(date.Year(), date.Month(), 1, 0, 0, 0, 0, time.UTC)
		next = start.AddDate(0, 1, 0)
	default:
		start = date
		next = start.AddDate(0, 0, 1)
	}

	return Period{Start: start, End: next.AddDate(0, 0, -1)}
}

func Next(periodStart time.Time, frequency string) time.Time {
	switch frequency {
	case Weekly:
		return day(periodStart).AddDate(0, 0, 7)
	case Fortnightly:
		return day(periodStart).AddDate(0, 0, 14)
	case Monthly:
		return time.Date(periodStart.Year(), periodStart.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	default:
		return day(periodStart).AddDate(0, 0, 1)
	}
}

func Previous(periodStart time.Time, frequency string) time.Time {
	switch frequency {
	case Weekly:
		return day(periodStart).AddDate(0, 0, -7)
	case Fortnightly:
		return day(periodStart).AddDate(0, 0, -14)
	case Monthly:
		return time.Date(periodStart.Year(), periodStart.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	default:
		return day(periodStart).AddDate(0, 0, -1)
	}
}

func Unit(frequency string) string {
	switch frequency {
	case Weekly:
		return "week"
	case Fortnightly:
		return "fortnight"
	case Monthly:
		return "month"
	default:
		return "day"
	}
}

func CompletedPeriods(logDates []string, frequency string) map[string]struct{} {
	out := make(map[string]struct{}, len(logDates))
	for _, value := range logDates {
		d, err := ParseDate(value)
		if err != nil {
			continue
		}
		out[ForDate(d, frequency).Start.Format("2006-01-02")] = struct{}{}
	}
	return out
}

// Streaks returns current and longest consecutive completed cadence periods.
// An unfinished current period is still open, so current begins at the prior
// period in that case instead of being reset prematurely.
func Streaks(logDates []string, frequency string, today time.Time) (current, longest int) {
	completed := CompletedPeriods(logDates, frequency)
	if len(completed) == 0 {
		return 0, 0
	}

	cursor := ForDate(today, frequency).Start
	if _, ok := completed[cursor.Format("2006-01-02")]; !ok {
		cursor = Previous(cursor, frequency)
	}
	for {
		if _, ok := completed[cursor.Format("2006-01-02")]; !ok {
			break
		}
		current++
		cursor = Previous(cursor, frequency)
	}

	starts := make([]time.Time, 0, len(completed))
	for key := range completed {
		d, err := ParseDate(key)
		if err == nil {
			starts = append(starts, d)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })

	run := 0
	var previous time.Time
	for _, start := range starts {
		if previous.IsZero() || Next(previous, frequency).Equal(start) {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
		previous = start
	}
	return current, longest
}

func CountOverlapping(from, to time.Time, frequency string) int {
	if to.Before(from) {
		return 0
	}
	cursor := ForDate(from, frequency).Start
	count := 0
	for !cursor.After(to) {
		count++
		cursor = Next(cursor, frequency)
	}
	return count
}

func weekStart(date time.Time) time.Time {
	offset := (int(date.Weekday()) + 6) % 7
	return date.AddDate(0, 0, -offset)
}

func day(date time.Time) time.Time {
	return time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
}
