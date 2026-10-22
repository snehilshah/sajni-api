// Package taskrepeat holds repeating tasks. A repeating task is one row whose
// dates move forward: when its day (or week, or month) ends it moves to its
// next date. Done reopens as todo, still open records a miss, blocked stays
// blocked (the caller notifies). Steps untick each time. Shared by the API
// (the sweep and Skip) and the AI tools so both apply the same rules.
package taskrepeat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
	"sajni/internal/reminder"
	"sajni/internal/reminderqueue"
)

// Rule is the reminders' repeat rule. Tasks never end on their own (no
// until or count): "Stop repeating" ends a series.
type Rule = reminder.Rule

var (
	ErrInvalid  = errors.New("invalid repeat")
	ErrNotFound = errors.New("task not found")
)

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

// Scope is where a task sits: a day (due_date), a week (week_of, the
// Monday) or a month (month_of, the 1st). Empty when undated.
type Scope struct {
	Due, Week, Month string
}

func (s Scope) anchor() (kind string, date time.Time, ok bool) {
	for _, c := range []struct{ kind, value string }{{"day", s.Due}, {"week", s.Week}, {"month", s.Month}} {
		if c.value == "" {
			continue
		}
		if d, err := time.Parse("2006-01-02", c.value[:min(len(c.value), 10)]); err == nil {
			return c.kind, d, true
		}
	}
	return "", time.Time{}, false
}

// Normalize checks a rule against the task's scope. Day tasks take any
// reminder rule; week tasks repeat every N weeks, month tasks every N months.
func Normalize(rule Rule, scope Scope) (Rule, error) {
	if !rule.Recurring() {
		return Rule{}, nil
	}
	kind, start, ok := scope.anchor()
	if !ok {
		return Rule{}, invalid("Give the task a day, week or month before it can repeat.")
	}
	rule.Until, rule.Count = "", 0
	switch kind {
	case "week":
		if rule.Frequency != "weekly" {
			return Rule{}, invalid("A week task repeats weekly.")
		}
		return interval(Rule{Frequency: "weekly", Interval: rule.Interval})
	case "month":
		if rule.Frequency != "monthly" {
			return Rule{}, invalid("A month task repeats monthly.")
		}
		return interval(Rule{Frequency: "monthly", Interval: rule.Interval})
	}
	out, err := reminder.Normalize(rule, start)
	if err != nil {
		return Rule{}, invalid(err.Error())
	}
	return out, nil
}

func interval(r Rule) (Rule, error) {
	if r.Interval == 0 {
		r.Interval = 1
	}
	if r.Interval < 1 || r.Interval > 999 {
		return Rule{}, invalid("Repeat every 1 to 999.")
	}
	return r, nil
}

// Decode reads a stored rule; anything unreadable is "doesn't repeat".
func Decode(raw []byte) Rule {
	var r Rule
	_ = json.Unmarshal(raw, &r)
	return r
}

// Encode is the stored form: {} when the task doesn't repeat.
func Encode(r Rule) string {
	if !r.Recurring() {
		return "{}"
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// Next is the series' first date after `from` that is on or after
// `notBefore`. For week and month tasks the dates are Mondays and 1sts.
func Next(rule Rule, kind string, from, notBefore time.Time) (time.Time, bool) {
	if !rule.Recurring() {
		return time.Time{}, false
	}
	step := rule.Interval
	if step < 1 {
		step = 1
	}
	cur := from
	for i := 0; i < 10000; i++ {
		switch kind {
		case "week":
			cur = cur.AddDate(0, 0, 7*step)
		case "month":
			cur = time.Date(cur.Year(), cur.Month()+time.Month(step), 1, 0, 0, 0, 0, time.UTC)
		default:
			next, ok := reminder.Next(from, cur, 1, rule, time.UTC)
			if !ok {
				return time.Time{}, false
			}
			cur = next
		}
		if !cur.Before(notBefore) {
			return cur, true
		}
	}
	return time.Time{}, false
}

// Moved is one task after it moved to its next date.
type Moved struct {
	ID      int64
	UserID  string
	Title   string
	From    string // the date it left (YYYY-MM-DD)
	To      string // its new date
	Blocked bool
	// What to re-enqueue: the task's own reminder and its extra reminders.
	ScheduledAt *time.Time
	Remind      bool
	Reminders   map[int64]time.Time
}

// Advance moves one repeating task to its next date on or after `today`
// (the user's local date). skip marks it skipped rather than lapsed: no
// miss is recorded. Runs inside tx; returns false when the task doesn't
// repeat, is scratched, or (for a lapse) its period hasn't ended yet.
func Advance(ctx context.Context, tx *sql.Tx, uid string, id int64, today time.Time, loc *time.Location, skip bool) (Moved, bool, error) {
	var (
		m                Moved
		status           string
		due, week, month sql.NullString
		scheduled        sql.NullTime
		raw, stepsRaw    []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT title, status, due_date::text, week_of::text, month_of::text, scheduled_at, remind, recurrence, steps
		  FROM tasks WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, uid).
		Scan(&m.Title, &status, &due, &week, &month, &scheduled, &m.Remind, &raw, &stepsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, ErrNotFound
	}
	if err != nil {
		return m, false, err
	}
	rule := Decode(raw)
	kind, from, ok := Scope{Due: due.String, Week: week.String, Month: month.String}.anchor()
	if !rule.Recurring() || !ok || status == "scratched" {
		return m, false, nil
	}
	// A lapse needs the whole period behind it: the day, the week or the month.
	notBefore := dateOf(today)
	switch kind {
	case "week":
		notBefore = notBefore.AddDate(0, 0, -((int(notBefore.Weekday()) + 6) % 7))
	case "month":
		notBefore = time.Date(notBefore.Year(), notBefore.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	if !skip && !from.Before(notBefore) {
		return m, false, nil
	}
	if skip && kind == "day" {
		// Skipping today's (or a later) one moves past it.
		notBefore = maxDate(notBefore, from.AddDate(0, 0, 1))
	}
	next, ok := Next(rule, kind, from, notBefore)
	if !ok {
		return m, false, nil
	}
	m.ID, m.UserID = id, uid
	m.From, m.To = from.Format("2006-01-02"), next.Format("2006-01-02")
	m.Blocked = status == "blocked"

	newStatus := "todo"
	if m.Blocked {
		newStatus = "blocked"
	}
	event := "repeated"
	switch {
	case skip:
		event = "skipped"
	case status != "done":
		event = "missed"
		if kind == "day" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO task_due_history (user_id, task_id, due_date, outcome)
				SELECT $1, $2, $3, 'missed'
				 WHERE NOT EXISTS (SELECT 1 FROM task_due_history WHERE user_id=$1 AND task_id=$2 AND due_date=$3)`,
				uid, id, m.From); err != nil {
				return m, false, err
			}
		}
	}

	days := int(next.Sub(from).Hours() / 24)
	var newScheduled any
	if scheduled.Valid && kind == "day" {
		t := shiftDays(scheduled.Time, days, loc)
		newScheduled, m.ScheduledAt = t, &t
	}
	var dueArg, weekArg, monthArg any
	switch kind {
	case "week":
		weekArg = m.To
	case "month":
		monthArg = m.To
	default:
		dueArg = m.To
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET due_date=$3, week_of=$4, month_of=$5, scheduled_at=$6, status=$7,
		       steps=$8::jsonb, reminded_at=NULL, reminder_claimed_until=NULL, updated_at=NOW()
		 WHERE id=$1 AND user_id=$2`,
		id, uid, dueArg, weekArg, monthArg, newScheduled, newStatus, untick(stepsRaw)); err != nil {
		return m, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events (user_id, task_id, kind, from_val, to_val) VALUES ($1,$2,$3,$4,$5)`,
		uid, id, event, m.From, m.To); err != nil {
		return m, false, err
	}

	// Extra reminders move with the task and fire again.
	rows, err := tx.QueryContext(ctx, `SELECT id, remind_at FROM task_reminders WHERE task_id=$1 AND user_id=$2`, id, uid)
	if err != nil {
		return m, false, err
	}
	m.Reminders = map[int64]time.Time{}
	for rows.Next() {
		var rid int64
		var at time.Time
		if rows.Scan(&rid, &at) == nil {
			m.Reminders[rid] = shiftDays(at, days, loc)
		}
	}
	rows.Close()
	for rid, at := range m.Reminders {
		if _, err := tx.ExecContext(ctx, `UPDATE task_reminders SET remind_at=$1, sent_at=NULL, claimed_until=NULL WHERE id=$2`, at, rid); err != nil {
			return m, false, err
		}
	}
	return m, true, nil
}

// Enqueue arms the moved task's reminders.
func Enqueue(ctx context.Context, queue reminderqueue.Queue, m Moved) {
	if m.Remind && m.ScheduledAt != nil {
		if err := queue.EnqueueTask(ctx, m.ID, *m.ScheduledAt); err != nil {
			log.Warn().Err(err).Int64("task", m.ID).Msg("repeat reminder enqueue failed")
		}
	}
	now := time.Now()
	for rid, at := range m.Reminders {
		if at.After(now) {
			if err := queue.EnqueueMulti(ctx, rid, at); err != nil {
				log.Warn().Err(err).Int64("reminder", rid).Msg("repeat reminder enqueue failed")
			}
		}
	}
}

// Skip moves a repeating task past its current date without a miss.
func Skip(ctx context.Context, d *db.DB, queue reminderqueue.Queue, uid string, id int64) (Moved, error) {
	loc := Location(ctx, d, uid)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Moved{}, err
	}
	defer tx.Rollback()
	m, ok, err := Advance(ctx, tx, uid, id, time.Now().In(loc), loc, true)
	if err != nil {
		return m, err
	}
	if !ok {
		return m, invalid("This task doesn't repeat.")
	}
	if err := tx.Commit(); err != nil {
		return m, err
	}
	Enqueue(ctx, queue, m)
	return m, nil
}

// Sweep moves every repeating task whose period ended in its owner's local
// calendar. Safe to run often: a task moves only once its date is past.
func Sweep(ctx context.Context, d *db.DB, queue reminderqueue.Queue) ([]Moved, error) {
	// A day of slack covers every timezone; Advance decides per user.
	rows, err := d.QueryContext(ctx, `
		SELECT id, user_id::text FROM tasks
		 WHERE recurrence <> '{}'::jsonb AND status <> 'scratched'
		   AND (due_date < CURRENT_DATE + 1 OR week_of < CURRENT_DATE + 1 OR month_of < CURRENT_DATE + 1)
		 ORDER BY id LIMIT 2000`)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id  int64
		uid string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.uid) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()
	locs := map[string]*time.Location{}
	var moved []Moved
	for _, c := range cands {
		loc, ok := locs[c.uid]
		if !ok {
			loc = Location(ctx, d, c.uid)
			locs[c.uid] = loc
		}
		m, ok, err := advanceOne(ctx, d, c.uid, c.id, loc)
		if err != nil {
			log.Warn().Err(err).Int64("task", c.id).Msg("repeat sweep failed")
			continue
		}
		if ok {
			Enqueue(ctx, queue, m)
			moved = append(moved, m)
		}
	}
	return moved, nil
}

func advanceOne(ctx context.Context, d *db.DB, uid string, id int64, loc *time.Location) (Moved, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Moved{}, false, err
	}
	defer tx.Rollback()
	m, ok, err := Advance(ctx, tx, uid, id, time.Now().In(loc), loc, false)
	if err != nil || !ok {
		return m, false, err
	}
	return m, true, tx.Commit()
}

// Location is the user's timezone (Asia/Kolkata when unset or unknown).
func Location(ctx context.Context, d *db.DB, uid string) *time.Location {
	var tz string
	_ = d.QueryRowContext(ctx, `SELECT COALESCE(timezone,'') FROM users WHERE id=$1`, uid).Scan(&tz)
	if loc, err := time.LoadLocation(tz); err == nil && tz != "" {
		return loc
	}
	loc, _ := time.LoadLocation("Asia/Kolkata")
	return loc
}

// dateOf is t's local calendar date as a UTC midnight, the form Next uses.
func dateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func maxDate(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// shiftDays keeps the local clock time while moving the calendar date.
func shiftDays(t time.Time, days int, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()+days, l.Hour(), l.Minute(), l.Second(), 0, loc)
}

// untick clears every step, keeping their text and order.
func untick(raw []byte) string {
	var steps []map[string]any
	if json.Unmarshal(raw, &steps) != nil || steps == nil {
		return "[]"
	}
	for _, s := range steps {
		s["done"] = false
	}
	b, _ := json.Marshal(steps)
	return string(b)
}

type runner interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Fit stores rule on a task after an edit, checked against the task's
// current scope. With no rule given, a stored one that no longer fits (the
// task lost its date, or changed scope) is dropped.
func Fit(ctx context.Context, q runner, uid string, id int64, rule *Rule) error {
	var due, week, month sql.NullString
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT due_date::text, week_of::text, month_of::text, recurrence FROM tasks WHERE id=$1 AND user_id=$2`, id, uid).
		Scan(&due, &week, &month, &raw); err != nil {
		return err
	}
	scope := Scope{Due: due.String, Week: week.String, Month: month.String}
	stored := Encode(Decode(raw))
	next := "{}"
	if rule != nil {
		r, err := Normalize(*rule, scope)
		if err != nil {
			return err
		}
		next = Encode(r)
	} else if r, err := Normalize(Decode(raw), scope); err == nil {
		next = Encode(r)
	}
	if next == stored {
		return nil
	}
	_, err := q.ExecContext(ctx, `UPDATE tasks SET recurrence=$1::jsonb, updated_at=NOW() WHERE id=$2 AND user_id=$3`, next, id, uid)
	return err
}
