package reminder

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Rule is the deliberately practical recurrence contract shared by API, web,
// Android, and AI. An empty Frequency means a one-time reminder.
type Rule struct {
	Frequency   string `json:"frequency,omitempty"`
	Interval    int    `json:"interval,omitempty"`
	Weekdays    []int  `json:"weekdays,omitempty"` // 0=Sunday … 6=Saturday
	MonthlyMode string `json:"monthly_mode,omitempty"`
	MonthDay    int    `json:"month_day,omitempty"`
	Weekday     int    `json:"weekday,omitempty"`
	Ordinal     int    `json:"ordinal,omitempty"` // 1…5 or -1 (last)
	Until       string `json:"until,omitempty"`   // local YYYY-MM-DD, inclusive
	Count       int    `json:"count,omitempty"`
}

func (r Rule) Recurring() bool { return r.Frequency != "" }

func Normalize(rule Rule, start time.Time) (Rule, error) {
	if rule.Frequency == "" {
		return Rule{}, nil
	}
	switch rule.Frequency {
	case "daily", "weekly", "monthly", "yearly":
	default:
		return Rule{}, fmt.Errorf("invalid recurrence frequency %q", rule.Frequency)
	}
	if rule.Interval == 0 {
		rule.Interval = 1
	}
	if rule.Interval < 1 || rule.Interval > 999 {
		return Rule{}, errors.New("recurrence interval must be between 1 and 999")
	}
	if rule.Count < 0 || rule.Count > 100000 {
		return Rule{}, errors.New("recurrence count is invalid")
	}
	if rule.Until != "" {
		if _, err := time.Parse("2006-01-02", rule.Until); err != nil {
			return Rule{}, errors.New("recurrence until must be YYYY-MM-DD")
		}
	}

	if rule.Frequency == "weekly" {
		if len(rule.Weekdays) == 0 {
			rule.Weekdays = []int{int(start.Weekday())}
		}
		seen := map[int]bool{}
		weekdays := make([]int, 0, len(rule.Weekdays))
		for _, weekday := range rule.Weekdays {
			if weekday < 0 || weekday > 6 {
				return Rule{}, errors.New("weekday must be between 0 and 6")
			}
			if !seen[weekday] {
				seen[weekday] = true
				weekdays = append(weekdays, weekday)
			}
		}
		sort.Ints(weekdays)
		rule.Weekdays = weekdays
	} else {
		rule.Weekdays = nil
	}

	if rule.Frequency == "monthly" {
		if rule.MonthlyMode == "" {
			rule.MonthlyMode = "date"
		}
		switch rule.MonthlyMode {
		case "date":
			if rule.MonthDay == 0 {
				rule.MonthDay = start.Day()
			}
			if rule.MonthDay < 1 || rule.MonthDay > 31 {
				return Rule{}, errors.New("month day must be between 1 and 31")
			}
		case "weekday":
			if rule.Weekday < 0 || rule.Weekday > 6 {
				return Rule{}, errors.New("monthly weekday must be between 0 and 6")
			}
			if rule.Ordinal == 0 {
				rule.Ordinal = ordinalInMonth(start)
			}
			if rule.Ordinal != -1 && (rule.Ordinal < 1 || rule.Ordinal > 5) {
				return Rule{}, errors.New("monthly ordinal must be 1 through 5 or -1")
			}
		default:
			return Rule{}, errors.New("monthly_mode must be date or weekday")
		}
	} else {
		rule.MonthlyMode, rule.MonthDay, rule.Weekday, rule.Ordinal = "", 0, 0, 0
	}
	return rule, nil
}

// Next returns the next canonical occurrence after current. sequence is the
// current 1-based occurrence number; snoozed fire times never enter this
// calculation, so a one-off delay cannot shift a recurring series.
func Next(start, current time.Time, sequence int, rule Rule, loc *time.Location) (time.Time, bool) {
	if !rule.Recurring() || (rule.Count > 0 && sequence >= rule.Count) {
		return time.Time{}, false
	}
	if loc == nil {
		loc = time.UTC
	}
	start, current = start.In(loc), current.In(loc)
	var next time.Time
	switch rule.Frequency {
	case "daily":
		next = current.AddDate(0, 0, rule.Interval)
	case "weekly":
		next = nextWeekly(start, current, rule, loc)
	case "monthly":
		next = nextMonthly(current, rule, loc)
	case "yearly":
		next = localDateTime(current.Year()+rule.Interval, start.Month(), start.Day(), start, loc)
	default:
		return time.Time{}, false
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	if rule.Until != "" && next.Format("2006-01-02") > rule.Until {
		return time.Time{}, false
	}
	return next, true
}

func nextWeekly(start, current time.Time, rule Rule, loc *time.Location) time.Time {
	startWeek := mondayDate(start, loc)
	for days := 1; days <= 3660; days++ {
		candidateDate := current.AddDate(0, 0, days)
		candidate := localDateTime(candidateDate.Year(), candidateDate.Month(), candidateDate.Day(), start, loc)
		week := mondayDate(candidate, loc)
		weekDelta := int(dateOnly(week).Sub(dateOnly(startWeek)).Hours() / 24 / 7)
		if weekDelta >= 0 && weekDelta%rule.Interval == 0 && containsInt(rule.Weekdays, int(candidate.Weekday())) {
			return candidate
		}
	}
	return time.Time{}
}

func nextMonthly(current time.Time, rule Rule, loc *time.Location) time.Time {
	for attempts := 1; attempts <= 1200; attempts++ {
		monthIndex := current.Year()*12 + int(current.Month()) - 1 + attempts*rule.Interval
		year, month := monthIndex/12, time.Month(monthIndex%12+1)
		if rule.MonthlyMode == "weekday" {
			if candidate, ok := nthWeekday(year, month, time.Weekday(rule.Weekday), rule.Ordinal, current, loc); ok {
				return candidate
			}
			continue
		}
		return localDateTime(year, month, rule.MonthDay, current, loc)
	}
	return time.Time{}
}

func nthWeekday(year int, month time.Month, weekday time.Weekday, ordinal int, clock time.Time, loc *time.Location) (time.Time, bool) {
	if ordinal == -1 {
		last := daysInMonth(year, month)
		for day := last; day >= last-6; day-- {
			candidate := localDateTime(year, month, day, clock, loc)
			if candidate.Weekday() == weekday {
				return candidate, true
			}
		}
	}
	first := localDateTime(year, month, 1, clock, loc)
	day := 1 + (int(weekday)-int(first.Weekday())+7)%7 + (ordinal-1)*7
	if day > daysInMonth(year, month) {
		return time.Time{}, false
	}
	return localDateTime(year, month, day, clock, loc), true
}

func localDateTime(year int, month time.Month, day int, clock time.Time, loc *time.Location) time.Time {
	if max := daysInMonth(year, month); day > max {
		day = max
	}
	return time.Date(year, month, day, clock.Hour(), clock.Minute(), clock.Second(), clock.Nanosecond(), loc)
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func mondayDate(t time.Time, loc *time.Location) time.Time {
	delta := (int(t.Weekday()) + 6) % 7
	d := t.AddDate(0, 0, -delta)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func ordinalInMonth(t time.Time) int {
	if t.Day()+7 > daysInMonth(t.Year(), t.Month()) {
		return -1
	}
	return (t.Day()-1)/7 + 1
}

func containsInt(values []int, value int) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
