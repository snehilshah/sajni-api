// Package routine holds Journal routines: a named time slot that repeats on
// chosen weekdays ("Office start", Mon–Fri 09:00 in the user's timezone).
// A routine carries a standing checklist (items), ticked per day and fresh
// each occurrence, and one-off agenda items, which are ordinary tasks with
// tasks.routine_id set and dated to an occurrence. Shared by the API and the
// AI tools so both apply the same rules.
package routine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
	"sajni/internal/reminderqueue"
)

var (
	ErrInvalid  = errors.New("invalid routine")
	ErrNotFound = errors.New("routine not found")
)

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

// Item is one line of a routine's standing checklist.
type Item struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Routine struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	Weekdays  []int  `json:"weekdays"`   // 0=Sunday … 6=Saturday
	StartTime string `json:"start_time"` // local HH:MM
	Notify    bool   `json:"notify"`
	Items     []Item `json:"items"`
	// NextAlertAt is the next alert instant (null with notify off).
	NextAlertAt *time.Time `json:"next_alert_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// SaveInput is the full editable shape; a save replaces every field.
type SaveInput struct {
	Name      string `json:"name"`
	Color     string `json:"color"`
	Weekdays  []int  `json:"weekdays"`
	StartTime string `json:"start_time"`
	Notify    *bool  `json:"notify"`
	Items     []Item `json:"items"`
}

var clockRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
var colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// Normalize validates input and fills defaults: workdays, 09:00, notify on.
func Normalize(in SaveInput) (SaveInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return in, invalid("Give the routine a name.")
	}
	if len([]rune(in.Name)) > 80 {
		return in, invalid("Keep the name under 80 characters.")
	}
	in.Color = strings.ToUpper(strings.TrimSpace(in.Color))
	if in.Color != "" && !colorRe.MatchString(in.Color) {
		return in, invalid("Colour must be a hex value like #2D5A4F.")
	}
	if in.Weekdays == nil {
		in.Weekdays = []int{1, 2, 3, 4, 5}
	}
	seen := map[int]bool{}
	days := make([]int, 0, len(in.Weekdays))
	for _, d := range in.Weekdays {
		if d < 0 || d > 6 {
			return in, invalid("Weekdays run 0 (Sunday) to 6 (Saturday).")
		}
		if !seen[d] {
			seen[d] = true
			days = append(days, d)
		}
	}
	if len(days) == 0 {
		return in, invalid("Pick at least one day.")
	}
	sort.Ints(days)
	in.Weekdays = days
	in.StartTime = strings.TrimSpace(in.StartTime)
	if in.StartTime == "" {
		in.StartTime = "09:00"
	}
	if len(in.StartTime) == 4 && in.StartTime[1] == ':' {
		in.StartTime = "0" + in.StartTime
	}
	if !clockRe.MatchString(in.StartTime) {
		return in, invalid("Start time must be HH:MM, like 09:00.")
	}
	if in.Notify == nil {
		on := true
		in.Notify = &on
	}
	items := make([]Item, 0, len(in.Items))
	ids := map[string]bool{}
	for i, it := range in.Items {
		it.Text = strings.TrimSpace(it.Text)
		if it.Text == "" {
			continue
		}
		if len([]rune(it.Text)) > 300 {
			return in, invalid("Keep each item under 300 characters.")
		}
		it.ID = strings.TrimSpace(it.ID)
		if it.ID == "" || ids[it.ID] {
			it.ID = fmt.Sprintf("i_%d_%d", time.Now().UnixMilli(), i)
		}
		ids[it.ID] = true
		items = append(items, it)
	}
	if len(items) > 50 {
		return in, invalid("A routine holds up to 50 items.")
	}
	in.Items = items
	return in, nil
}

// On reports whether the routine runs on date's weekday.
func On(weekdays []int, date time.Time) bool {
	for _, d := range weekdays {
		if d == int(date.Weekday()) {
			return true
		}
	}
	return false
}

// At is the routine's start on the given local date.
func At(startTime string, date time.Time, loc *time.Location) time.Time {
	var h, m int
	fmt.Sscanf(startTime, "%d:%d", &h, &m)
	return time.Date(date.Year(), date.Month(), date.Day(), h, m, 0, 0, loc)
}

// Next is the first occurrence strictly after `after`, or false when the
// routine has no days (never, after Normalize).
func Next(weekdays []int, startTime string, loc *time.Location, after time.Time) (time.Time, bool) {
	local := after.In(loc)
	for i := 0; i <= 7; i++ {
		day := local.AddDate(0, 0, i)
		if !On(weekdays, day) {
			continue
		}
		if at := At(startTime, day, loc); at.After(after) {
			return at, true
		}
	}
	return time.Time{}, false
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

const cols = `id, name, color, weekdays, start_time, notify, items, next_alert_at, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Routine, error) {
	var r Routine
	var days, items []byte
	var next sql.NullTime
	if err := row.Scan(&r.ID, &r.Name, &r.Color, &days, &r.StartTime, &r.Notify, &items, &next, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	_ = json.Unmarshal(days, &r.Weekdays)
	_ = json.Unmarshal(items, &r.Items)
	if r.Weekdays == nil {
		r.Weekdays = []int{}
	}
	if r.Items == nil {
		r.Items = []Item{}
	}
	if next.Valid {
		r.NextAlertAt = &next.Time
	}
	return r, nil
}

// List returns every routine, earliest in the day first.
func List(ctx context.Context, d *db.DB, uid string) ([]Routine, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+cols+` FROM routines WHERE user_id=$1 ORDER BY start_time, id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Routine{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func Get(ctx context.Context, d *db.DB, uid string, id int64) (Routine, error) {
	r, err := scan(d.QueryRowContext(ctx, `SELECT `+cols+` FROM routines WHERE user_id=$1 AND id=$2`, uid, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// Save creates (id 0) or replaces a routine and re-arms its alert.
func Save(ctx context.Context, d *db.DB, queue reminderqueue.Queue, uid string, id int64, in SaveInput) (Routine, error) {
	in, err := Normalize(in)
	if err != nil {
		return Routine{}, err
	}
	days, _ := json.Marshal(in.Weekdays)
	items, _ := json.Marshal(in.Items)
	var next any
	if *in.Notify {
		if at, ok := Next(in.Weekdays, in.StartTime, Location(ctx, d, uid), time.Now()); ok {
			next = at
		}
	}
	if id == 0 {
		err = d.QueryRowContext(ctx, `
			INSERT INTO routines (user_id, name, color, weekdays, start_time, notify, items, next_alert_at)
			VALUES ($1,$2,$3,$4::jsonb,$5,$6,$7::jsonb,$8) RETURNING id`,
			uid, in.Name, in.Color, string(days), in.StartTime, *in.Notify, string(items), next).Scan(&id)
	} else {
		var res sql.Result
		res, err = d.ExecContext(ctx, `
			UPDATE routines SET name=$3, color=$4, weekdays=$5::jsonb, start_time=$6, notify=$7, items=$8::jsonb,
			       next_alert_at=$9, alert_claimed_until=NULL, updated_at=NOW()
			 WHERE user_id=$1 AND id=$2`,
			uid, id, in.Name, in.Color, string(days), in.StartTime, *in.Notify, string(items), next)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return Routine{}, ErrNotFound
			}
		}
	}
	if err != nil {
		return Routine{}, err
	}
	if at, ok := next.(time.Time); ok {
		enqueue(ctx, queue, id, at)
	}
	return Get(ctx, d, uid, id)
}

// Delete removes a routine; its agenda tasks stay as plain tasks.
func Delete(ctx context.Context, d *db.DB, uid string, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM routines WHERE user_id=$1 AND id=$2`, uid, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Agenda is the brief task shape listed under a routine's day.
type Agenda struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// DayRoutine is one routine as it stands on one date.
type DayRoutine struct {
	Routine
	Date   string   `json:"date"`
	Done   []string `json:"done"`   // standing item ids ticked that day
	Agenda []Agenda `json:"agenda"` // tasks for that occurrence
}

// Day lists the routines that run on date (YYYY-MM-DD), in time order, with
// that day's ticks and agenda tasks.
func Day(ctx context.Context, d *db.DB, uid, date string) ([]DayRoutine, error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, invalid("date must be YYYY-MM-DD")
	}
	all, err := List(ctx, d, uid)
	if err != nil {
		return nil, err
	}
	out := []DayRoutine{}
	byID := map[int64]int{}
	for _, r := range all {
		if !On(r.Weekdays, day) {
			continue
		}
		byID[r.ID] = len(out)
		out = append(out, DayRoutine{Routine: r, Date: date, Done: []string{}, Agenda: []Agenda{}})
	}
	if len(out) == 0 {
		return out, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT routine_id, done FROM routine_runs WHERE user_id=$1 AND run_date=$2`, uid, date)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rid int64
		var raw []byte
		if rows.Scan(&rid, &raw) == nil {
			if i, ok := byID[rid]; ok {
				_ = json.Unmarshal(raw, &out[i].Done)
			}
		}
	}
	rows.Close()
	rows, err = d.QueryContext(ctx, `
		SELECT routine_id, id, title, status, priority FROM tasks
		 WHERE user_id=$1 AND routine_id IS NOT NULL AND due_date=$2 AND status <> 'scratched'
		 ORDER BY (status = 'done'), created_at, id`, uid, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rid int64
		var a Agenda
		if rows.Scan(&rid, &a.ID, &a.Title, &a.Status, &a.Priority) == nil {
			if i, ok := byID[rid]; ok {
				out[i].Agenda = append(out[i].Agenda, a)
			}
		}
	}
	return out, rows.Err()
}

// Check ticks (or unticks) one standing item for one date.
func Check(ctx context.Context, d *db.DB, uid string, id int64, date, itemID string, done bool) error {
	r, err := Get(ctx, d, uid, id)
	if err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return invalid("date must be YYYY-MM-DD")
	}
	known := false
	for _, it := range r.Items {
		known = known || it.ID == itemID
	}
	if !known {
		return invalid("That item isn't on this routine any more.")
	}
	// One statement, so two quick taps on different items can't lose one.
	if done {
		_, err = d.ExecContext(ctx, `
			INSERT INTO routine_runs (routine_id, user_id, run_date, done) VALUES ($1,$2,$3,jsonb_build_array($4::text))
			ON CONFLICT (routine_id, run_date) DO UPDATE
			   SET done = CASE WHEN routine_runs.done ? $4 THEN routine_runs.done ELSE routine_runs.done || jsonb_build_array($4::text) END,
			       updated_at = NOW()`, id, uid, date, itemID)
	} else {
		_, err = d.ExecContext(ctx, `
			UPDATE routine_runs SET done = done - $4::text, updated_at = NOW()
			 WHERE routine_id=$1 AND user_id=$2 AND run_date=$3`, id, uid, date, itemID)
	}
	return err
}

// Occurrence resolves where an agenda task lands: on `date` when given (a
// day the routine runs), else the routine's next occurrence after now.
// Returns the local date and the start instant.
func Occurrence(ctx context.Context, d *db.DB, uid string, id int64, date string) (string, time.Time, error) {
	r, err := Get(ctx, d, uid, id)
	if err != nil {
		return "", time.Time{}, err
	}
	loc := Location(ctx, d, uid)
	if date != "" {
		day, err := time.ParseInLocation("2006-01-02", date, loc)
		if err != nil {
			return "", time.Time{}, invalid("date must be YYYY-MM-DD")
		}
		if !On(r.Weekdays, day) {
			return "", time.Time{}, invalid(r.Name + " doesn't run on " + day.Format("Monday") + "s.")
		}
		return date, At(r.StartTime, day, loc), nil
	}
	at, ok := Next(r.Weekdays, r.StartTime, loc, time.Now())
	if !ok {
		return "", time.Time{}, invalid("This routine has no days set.")
	}
	return at.In(loc).Format("2006-01-02"), at, nil
}

// enqueue schedules the exact alert. Failure is logged, not returned: the
// row is committed and the daily sweep re-arms a lapsed alert.
func enqueue(ctx context.Context, queue reminderqueue.Queue, id int64, at time.Time) {
	if err := queue.EnqueueRoutine(ctx, id, at); err != nil {
		log.Warn().Err(err).Int64("routine", id).Msg("routine alert enqueue failed")
	}
}

// Advance moves a routine's alert to its next occurrence after `after` and
// enqueues it. Called once an alert is sent, or found stale by the sweep.
func Advance(ctx context.Context, d *db.DB, queue reminderqueue.Queue, id int64, after time.Time) error {
	var uid, start string
	var days []byte
	var notify bool
	if err := d.QueryRowContext(ctx, `SELECT user_id::text, weekdays, start_time, notify FROM routines WHERE id=$1`, id).
		Scan(&uid, &days, &start, &notify); err != nil {
		return err
	}
	var weekdays []int
	_ = json.Unmarshal(days, &weekdays)
	var next any
	if notify {
		if at, ok := Next(weekdays, start, Location(ctx, d, uid), after); ok {
			next = at
		}
	}
	if _, err := d.ExecContext(ctx, `UPDATE routines SET next_alert_at=$2, alert_claimed_until=NULL WHERE id=$1`, id, next); err != nil {
		return err
	}
	if at, ok := next.(time.Time); ok {
		enqueue(ctx, queue, id, at)
	}
	return nil
}
