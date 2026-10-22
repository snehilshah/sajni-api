// Package plan holds Journal plans: a named moment that repeats on chosen
// weekdays ("Office start", Mon–Fri 09:00 in the user's timezone) to plan
// what that day is about. A plan's tasks are ordinary tasks with
// tasks.plan_id set and dated to an occurrence; unfinished ones stay as
// overdue tasks and are offered again as "left over". Shared by the API and
// the AI tools so both apply the same rules.
package plan

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
	ErrInvalid  = errors.New("invalid plan")
	ErrNotFound = errors.New("plan not found")
)

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

type Plan struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	Weekdays  []int  `json:"weekdays"`   // 0=Sunday … 6=Saturday
	StartTime string `json:"start_time"` // local HH:MM
	Notify    bool   `json:"notify"`
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
}

var clockRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
var colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// Normalize validates input and fills defaults: workdays, 09:00, notify on.
func Normalize(in SaveInput) (SaveInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return in, invalid("Give the plan a name.")
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
	return in, nil
}

// On reports whether the plan runs on date's weekday.
func On(weekdays []int, date time.Time) bool {
	for _, d := range weekdays {
		if d == int(date.Weekday()) {
			return true
		}
	}
	return false
}

// At is the plan's start on the given local date.
func At(startTime string, date time.Time, loc *time.Location) time.Time {
	var h, m int
	fmt.Sscanf(startTime, "%d:%d", &h, &m)
	return time.Date(date.Year(), date.Month(), date.Day(), h, m, 0, 0, loc)
}

// Next is the first occurrence strictly after `after`, or false when the
// plan has no days (never, after Normalize).
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

const cols = `id, name, color, weekdays, start_time, notify, next_alert_at, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Plan, error) {
	var p Plan
	var days []byte
	var next sql.NullTime
	if err := row.Scan(&p.ID, &p.Name, &p.Color, &days, &p.StartTime, &p.Notify, &next, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return p, err
	}
	_ = json.Unmarshal(days, &p.Weekdays)
	if p.Weekdays == nil {
		p.Weekdays = []int{}
	}
	if next.Valid {
		p.NextAlertAt = &next.Time
	}
	return p, nil
}

// List returns every plan, earliest in the day first.
func List(ctx context.Context, d *db.DB, uid string) ([]Plan, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+cols+` FROM plans WHERE user_id=$1 ORDER BY start_time, id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func Get(ctx context.Context, d *db.DB, uid string, id int64) (Plan, error) {
	p, err := scan(d.QueryRowContext(ctx, `SELECT `+cols+` FROM plans WHERE user_id=$1 AND id=$2`, uid, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// Save creates (id 0) or replaces a plan and re-arms its alert.
func Save(ctx context.Context, d *db.DB, queue reminderqueue.Queue, uid string, id int64, in SaveInput) (Plan, error) {
	in, err := Normalize(in)
	if err != nil {
		return Plan{}, err
	}
	days, _ := json.Marshal(in.Weekdays)
	var next any
	if *in.Notify {
		if at, ok := Next(in.Weekdays, in.StartTime, Location(ctx, d, uid), time.Now()); ok {
			next = at
		}
	}
	if id == 0 {
		err = d.QueryRowContext(ctx, `
			INSERT INTO plans (user_id, name, color, weekdays, start_time, notify, next_alert_at)
			VALUES ($1,$2,$3,$4::jsonb,$5,$6,$7) RETURNING id`,
			uid, in.Name, in.Color, string(days), in.StartTime, *in.Notify, next).Scan(&id)
	} else {
		var res sql.Result
		res, err = d.ExecContext(ctx, `
			UPDATE plans SET name=$3, color=$4, weekdays=$5::jsonb, start_time=$6, notify=$7,
			       next_alert_at=$8, alert_claimed_until=NULL, updated_at=NOW()
			 WHERE user_id=$1 AND id=$2`,
			uid, id, in.Name, in.Color, string(days), in.StartTime, *in.Notify, next)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return Plan{}, ErrNotFound
			}
		}
	}
	if err != nil {
		return Plan{}, err
	}
	if at, ok := next.(time.Time); ok {
		enqueue(ctx, queue, id, at)
	}
	return Get(ctx, d, uid, id)
}

// Delete removes a plan; its tasks stay as plain tasks.
func Delete(ctx context.Context, d *db.DB, uid string, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM plans WHERE user_id=$1 AND id=$2`, uid, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Brief is the short task shape listed under a plan or in the picker.
type Brief struct {
	ID        int64   `json:"id"`
	Title     string  `json:"title"`
	Status    string  `json:"status"`
	Priority  string  `json:"priority"`
	DueDate   *string `json:"due_date"`
	Recurring bool    `json:"recurring"`
}

const briefCols = `t.id, t.title, t.status, t.priority, t.due_date::text, t.recurrence <> '{}'::jsonb`

func scanBriefs(rows *sql.Rows, err error) ([]Brief, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Brief{}
	for rows.Next() {
		var b Brief
		if err := rows.Scan(&b.ID, &b.Title, &b.Status, &b.Priority, &b.DueDate, &b.Recurring); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DayPlan is one plan as it stands on one date.
type DayPlan struct {
	Plan
	Date  string  `json:"date"`
	Tasks []Brief `json:"tasks"` // planned for this occurrence
	// LeftOver: this plan's unfinished tasks from earlier days, offered
	// again from today on.
	LeftOver []Brief `json:"left_over"`
}

// Day lists the plans that run on date (YYYY-MM-DD), in time order, with
// that occurrence's tasks.
func Day(ctx context.Context, d *db.DB, uid, date string) ([]DayPlan, error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, invalid("date must be YYYY-MM-DD")
	}
	all, err := List(ctx, d, uid)
	if err != nil {
		return nil, err
	}
	today := time.Now().In(Location(ctx, d, uid)).Format("2006-01-02")
	out := []DayPlan{}
	for _, p := range all {
		if !On(p.Weekdays, day) {
			continue
		}
		dp := DayPlan{Plan: p, Date: date}
		if dp.Tasks, err = scanBriefs(d.QueryContext(ctx, `
			SELECT `+briefCols+` FROM tasks t
			 WHERE t.user_id=$1 AND t.plan_id=$2 AND t.due_date=$3 AND t.status <> 'scratched'
			 ORDER BY (t.status = 'done'), t.created_at, t.id`, uid, p.ID, date)); err != nil {
			return nil, err
		}
		dp.LeftOver = []Brief{}
		if date >= today {
			if dp.LeftOver, err = scanBriefs(d.QueryContext(ctx, `
				SELECT `+briefCols+` FROM tasks t
				 WHERE t.user_id=$1 AND t.plan_id=$2 AND t.due_date < $3 AND t.status NOT IN ('done','scratched')
				 ORDER BY t.due_date DESC, t.id`, uid, p.ID, today)); err != nil {
				return nil, err
			}
		}
		out = append(out, dp)
	}
	return out, nil
}

// Group is one section of the planner's "pick a task" list.
type Group struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Tasks []Brief `json:"tasks"`
}

// Candidates are the open tasks worth pulling into a plan's occurrence on
// date: its left-overs, anything overdue, due that day, this week's, coming
// up, and undated. Tasks already in a plan that day are left out.
func Candidates(ctx context.Context, d *db.DB, uid string, id int64, date string) ([]Group, error) {
	if _, err := Get(ctx, d, uid, id); err != nil {
		return nil, err
	}
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, invalid("date must be YYYY-MM-DD")
	}
	now := time.Now().In(Location(ctx, d, uid))
	today := now.Format("2006-01-02")
	monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)).Format("2006-01-02")
	thisMonday := now.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7)).Format("2006-01-02")
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")

	// Open, not a month goal's session (those roll up to the goal).
	const open = `t.user_id=$1 AND t.status NOT IN ('done','scratched')
		AND NOT EXISTS (SELECT 1 FROM tasks g WHERE g.id=t.parent_task_id AND g.month_of IS NOT NULL)`
	specs := []struct {
		key, label, where string
		args              []any
	}{
		{"left_over", "Left over", `t.plan_id=$2 AND t.due_date < $3`, []any{id, today}},
		{"overdue", "Overdue", `(t.plan_id IS NULL OR t.plan_id<>$2) AND (t.due_date < $3 OR t.week_of < $4 OR t.month_of < $5)`, []any{id, today, thisMonday, thisMonth}},
		{"due", "Due that day", `t.plan_id IS NULL AND t.due_date = $2`, []any{date}},
		{"week", "That week", `t.week_of = $2`, []any{monday}},
		{"later", "Coming up", `t.due_date > $2 AND t.plan_id IS NULL`, []any{date}},
		{"undated", "No date", `t.due_date IS NULL AND t.week_of IS NULL AND t.month_of IS NULL AND t.parent_task_id IS NULL`, nil},
	}
	out := []Group{}
	seen := map[int64]bool{}
	for _, s := range specs {
		order := `t.updated_at DESC, t.id DESC`
		if s.key == "later" {
			order = `t.due_date, t.id`
		}
		args := append([]any{uid}, s.args...)
		tasks, err := scanBriefs(d.QueryContext(ctx, `SELECT `+briefCols+` FROM tasks t WHERE `+open+` AND `+s.where+
			` ORDER BY `+order+` LIMIT 30`, args...))
		if err != nil {
			return nil, err
		}
		g := Group{Key: s.key, Label: s.label, Tasks: []Brief{}}
		for _, t := range tasks {
			if !seen[t.ID] {
				seen[t.ID] = true
				g.Tasks = append(g.Tasks, t)
			}
		}
		if len(g.Tasks) > 0 {
			out = append(out, g)
		}
	}
	return out, nil
}

// Occurrence resolves where a plan task lands: on `date` when given (a day
// the plan runs), else the plan's next occurrence after now. Returns the
// local date and the start instant.
func Occurrence(ctx context.Context, d *db.DB, uid string, id int64, date string) (string, time.Time, error) {
	p, err := Get(ctx, d, uid, id)
	if err != nil {
		return "", time.Time{}, err
	}
	loc := Location(ctx, d, uid)
	if date != "" {
		day, err := time.ParseInLocation("2006-01-02", date, loc)
		if err != nil {
			return "", time.Time{}, invalid("date must be YYYY-MM-DD")
		}
		if !On(p.Weekdays, day) {
			return "", time.Time{}, invalid(p.Name + " doesn't run on " + day.Format("Monday") + "s.")
		}
		return date, At(p.StartTime, day, loc), nil
	}
	at, ok := Next(p.Weekdays, p.StartTime, loc, time.Now())
	if !ok {
		return "", time.Time{}, invalid("This plan has no days set.")
	}
	return at.In(loc).Format("2006-01-02"), at, nil
}

// enqueue schedules the exact alert. Failure is logged, not returned: the
// row is committed and the daily sweep re-arms a lapsed alert.
func enqueue(ctx context.Context, queue reminderqueue.Queue, id int64, at time.Time) {
	if err := queue.EnqueuePlan(ctx, id, at); err != nil {
		log.Warn().Err(err).Int64("plan", id).Msg("plan alert enqueue failed")
	}
}

// Advance moves a plan's alert to its next occurrence after `after` and
// enqueues it. Called once an alert is sent, or found stale by the sweep.
func Advance(ctx context.Context, d *db.DB, queue reminderqueue.Queue, id int64, after time.Time) error {
	var uid, start string
	var days []byte
	var notify bool
	if err := d.QueryRowContext(ctx, `SELECT user_id::text, weekdays, start_time, notify FROM plans WHERE id=$1`, id).
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
	if _, err := d.ExecContext(ctx, `UPDATE plans SET next_alert_at=$2, alert_claimed_until=NULL WHERE id=$1`, id, next); err != nil {
		return err
	}
	if at, ok := next.(time.Time); ok {
		enqueue(ctx, queue, id, at)
	}
	return nil
}

// AddTask pulls an existing task into the plan's occurrence on date (its
// next one when date is empty). A task moved off a past day is recorded as
// rescheduled, like any other move; a timed task keeps its clock time.
// Returns the date it landed on.
func AddTask(ctx context.Context, d *db.DB, queue reminderqueue.Queue, uid string, id, taskID int64, date string) (string, error) {
	date, _, err := Occurrence(ctx, d, uid, id, date)
	if err != nil {
		return "", err
	}
	loc := Location(ctx, d, uid)
	today := time.Now().In(loc).Format("2006-01-02")
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var due sql.NullString
	var scheduled sql.NullTime
	var status, name string
	var remind bool
	err = tx.QueryRowContext(ctx, `SELECT due_date::text, scheduled_at, status, remind FROM tasks WHERE id=$1 AND user_id=$2 FOR UPDATE`, taskID, uid).
		Scan(&due, &scheduled, &status, &remind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", invalid("That task no longer exists.")
	}
	if err != nil {
		return "", err
	}
	_ = tx.QueryRowContext(ctx, `SELECT name FROM plans WHERE id=$1`, id).Scan(&name)
	var newScheduled any
	if scheduled.Valid {
		l := scheduled.Time.In(loc)
		day, _ := time.ParseInLocation("2006-01-02", date, loc)
		at := time.Date(day.Year(), day.Month(), day.Day(), l.Hour(), l.Minute(), 0, 0, loc)
		newScheduled = at
	}
	if due.String != date {
		if due.Valid && due.String < today && status != "done" && status != "scratched" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO task_due_history (user_id, task_id, due_date, outcome)
				SELECT $1, $2, $3, 'rescheduled'
				 WHERE NOT EXISTS (SELECT 1 FROM task_due_history WHERE user_id=$1 AND task_id=$2 AND due_date=$3)`,
				uid, taskID, due.String); err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE task_due_history SET outcome='rescheduled' WHERE user_id=$1 AND task_id=$2 AND due_date=$3`,
				uid, taskID, due.String); err != nil {
				return "", err
			}
		}
		if due.Valid {
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_events (user_id, task_id, kind, from_val, to_val) VALUES ($1,$2,'rescheduled',$3,$4)`,
				uid, taskID, due.String, date); err != nil {
				return "", err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET plan_id=$3, due_date=$4, week_of=NULL, month_of=NULL, scheduled_at=$5,
		       reminded_at = CASE WHEN scheduled_at IS DISTINCT FROM $5::timestamptz THEN NULL ELSE reminded_at END,
		       updated_at=NOW()
		 WHERE id=$1 AND user_id=$2`, taskID, uid, id, date, newScheduled); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_events (user_id, task_id, kind, from_val, to_val) VALUES ($1,$2,'planned','',$3)`,
		uid, taskID, name+" · "+date); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if at, ok := newScheduled.(time.Time); ok && remind && at.After(time.Now()) && status != "done" && status != "scratched" {
		if err := queue.EnqueueTask(ctx, taskID, at); err != nil {
			log.Warn().Err(err).Int64("task", taskID).Msg("plan task reminder enqueue failed")
		}
	}
	return date, nil
}
