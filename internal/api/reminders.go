package api

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
)

// Reminders ride on tasks: a task with remind=TRUE and a scheduled_at gets
// one email RemindLead before its event time. The send is driven by an
// external trigger (Cloud Scheduler */5 → POST /internal/reminders/run),
// not an in-process ticker, because Cloud Run scales to zero between
// requests. reminded_at is the idempotency gate.

// RemindLead is how far ahead of scheduled_at the email goes out. Fixed by
// product spec; promote to a per-task column later without a data migration
// (default the column to this value).
const RemindLead = 5 * time.Minute

// reminderGrace is the lower bound on how stale a reminder may be and still
// send. Without it, recovering from a multi-hour outage would blast every
// reminder whose window elapsed while we were down.
const reminderGrace = 30 * time.Minute

// defaultTZ is the fallback zone when a user's timezone is unset/unparseable.
// Every Sajni user is IST (see the users.timezone backfill in db.migrate), so
// UTC was the wrong fallback — it shifted reminder/month-boundary clock times
// by 5.5h for any NULL row. Asia/Kolkata is the canonical name for IST.
const defaultTZ = "Asia/Kolkata"

// defaultLoc is defaultTZ resolved once at startup (tzdata is embedded in the
// binary). Falls back to UTC only if the zone somehow fails to load.
var defaultLoc = func() *time.Location {
	if l, err := time.LoadLocation(defaultTZ); err == nil {
		return l
	}
	return time.UTC
}()

// RegisterReminderCronHandler mounts the unauthenticated webhook Cloud
// Scheduler hits every 5 minutes. Header X-Reminder-Cron must match
// REMINDER_CRON_SECRET; the handler 401s without it. Mirrors the insight
// cron. Call from main once the root mux exists.
func RegisterReminderCronHandler(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /internal/reminders/run", reminderCronHandler(deps))
}

func reminderCronHandler(deps Deps) http.HandlerFunc {
	expected := os.Getenv("REMINDER_CRON_SECRET")
	return func(w http.ResponseWriter, r *http.Request) {
		if expected == "" || r.Header.Get("X-Reminder-Cron") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		n, err := ProcessReminderCron(r.Context(), deps)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, 200, map[string]int{"sent": n})
	}
}

// ProcessReminderCron emails every due, un-sent task reminder and stamps
// reminded_at. Idempotent: a row is only ever picked up once because the
// stamp drops it out of the window. A failed send leaves reminded_at NULL
// so the next tick retries.
func ProcessReminderCron(ctx context.Context, deps Deps) (int, error) {
	d := deps.DB
	if deps.Auth == nil {
		return 0, nil // no mailer configured
	}

	// Window: send-time (scheduled_at - lead) has arrived, the event isn't
	// stale beyond the grace floor, the task is still open, and we haven't
	// sent yet. make_interval keeps the lead a bound param, not string-built.
	rows, err := d.QueryContext(ctx, `
		SELECT t.id, t.title, t.scheduled_at, u.email, u.name, COALESCE(u.timezone,'')
		  FROM tasks t
		  JOIN users u ON u.id = t.user_id
		 WHERE t.remind = TRUE
		   AND t.reminded_at IS NULL
		   AND t.status NOT IN ('done','scratched')
		   AND t.scheduled_at IS NOT NULL
		   AND t.scheduled_at <= NOW() + make_interval(secs => $1)
		   AND t.scheduled_at >= NOW() - make_interval(secs => $2)
		   AND u.deleted_at IS NULL`,
		int(RemindLead.Seconds()), int(reminderGrace.Seconds()))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type due struct {
		id          int64
		title       string
		scheduledAt time.Time
		email, name string
		tz          string
	}
	var pending []due
	for rows.Next() {
		var x due
		if err := rows.Scan(&x.id, &x.title, &x.scheduledAt, &x.email, &x.name, &x.tz); err != nil {
			continue
		}
		pending = append(pending, x)
	}
	rows.Close()

	sent := 0
	for _, x := range pending {
		whenLabel := formatReminderWhen(x.scheduledAt, x.tz)
		// Fall back to the email as the greeting name for users who never
		// set a display name (name defaults to '' in the schema).
		name := x.name
		if name == "" {
			name = x.email
		}
		if err := deps.Auth.SendTaskReminder(ctx, x.email, name, x.title, whenLabel, "/tasks"); err != nil {
			// Leave reminded_at NULL so the next tick retries this one.
			log.Warn().Err(err).Int64("task", x.id).Msg("reminder email failed")
			continue
		}
		if _, err := d.ExecContext(ctx, `UPDATE tasks SET reminded_at = NOW() WHERE id = $1`, x.id); err != nil {
			log.Warn().Err(err).Int64("task", x.id).Msg("reminder stamp failed")
			continue
		}
		sent++
	}

	// Second pass: explicit multi-reminders (task_reminders). These fire AT
	// their own remind_at (no lead) and can sit on any date, independent of
	// the task's own time. sent_at is the idempotency stamp.
	sent += processTaskReminders(ctx, deps)
	return sent, nil
}

// processTaskReminders emails every due, un-sent row in task_reminders and
// stamps sent_at. Same window/grace/idempotency model as the legacy path.
func processTaskReminders(ctx context.Context, deps Deps) int {
	d := deps.DB
	rows, err := d.QueryContext(ctx, `
		SELECT r.id, t.id, t.title, t.scheduled_at, r.remind_at,
		       u.email, u.name, COALESCE(u.timezone,'')
		  FROM task_reminders r
		  JOIN tasks t ON t.id = r.task_id AND t.user_id = r.user_id
		  JOIN users u ON u.id = r.user_id
		 WHERE r.sent_at IS NULL
		   AND t.status NOT IN ('done','scratched')
		   AND r.remind_at <= NOW()
		   AND r.remind_at >= NOW() - make_interval(secs => $1)
		   AND u.deleted_at IS NULL`,
		int(reminderGrace.Seconds()))
	if err != nil {
		log.Warn().Err(err).Msg("task_reminders query failed")
		return 0
	}
	defer rows.Close()

	type due struct {
		rid, tid    int64
		title       string
		scheduledAt sql.NullTime
		remindAt    time.Time
		email, name string
		tz          string
	}
	var pending []due
	for rows.Next() {
		var x due
		if err := rows.Scan(&x.rid, &x.tid, &x.title, &x.scheduledAt, &x.remindAt, &x.email, &x.name, &x.tz); err != nil {
			continue
		}
		pending = append(pending, x)
	}
	rows.Close()

	sent := 0
	for _, x := range pending {
		// Anchor the "when" phrase to the task's own time if it has one,
		// otherwise to this reminder's instant.
		when := x.remindAt
		if x.scheduledAt.Valid {
			when = x.scheduledAt.Time
		}
		name := x.name
		if name == "" {
			name = x.email
		}
		if err := deps.Auth.SendTaskReminder(ctx, x.email, name, x.title, formatReminderWhen(when, x.tz), "/tasks"); err != nil {
			log.Warn().Err(err).Int64("reminder", x.rid).Msg("task reminder email failed")
			continue
		}
		if _, err := d.ExecContext(ctx, `UPDATE task_reminders SET sent_at = NOW() WHERE id = $1`, x.rid); err != nil {
			log.Warn().Err(err).Int64("reminder", x.rid).Msg("task reminder stamp failed")
			continue
		}
		sent++
	}
	return sent
}

// --- task reminder CRUD ----------------------------------------------------

type taskReminderRow struct {
	ID       int64   `json:"id"`
	RemindAt string  `json:"remind_at"`
	SentAt   *string `json:"sent_at"`
}

// listTaskReminders returns a task's explicit reminders, soonest first.
func listTaskReminders(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		rows, err := d.Query(
			`SELECT id, remind_at::text, sent_at::text FROM task_reminders
			 WHERE user_id = $1 AND task_id = $2 ORDER BY remind_at ASC`, uid, id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []taskReminderRow{}
		for rows.Next() {
			var x taskReminderRow
			rows.Scan(&x.ID, &x.RemindAt, &x.SentAt)
			out = append(out, x)
		}
		writeJSON(w, 200, out)
	}
}

// addTaskReminder inserts one reminder instant for a task the user owns.
func addTaskReminder(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var owned int
		d.QueryRow("SELECT 1 FROM tasks WHERE id = $1 AND user_id = $2", id, uid).Scan(&owned)
		if owned != 1 {
			errJSON(w, 404, "task not found")
			return
		}
		var body struct {
			RemindAt string `json:"remind_at"`
		}
		if err := readJSON(r, &body); err != nil || body.RemindAt == "" {
			errJSON(w, 400, "remind_at required")
			return
		}
		var rid int64
		if err := d.QueryRow(
			`INSERT INTO task_reminders (user_id, task_id, remind_at) VALUES ($1, $2, $3) RETURNING id`,
			uid, id, body.RemindAt,
		).Scan(&rid); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": rid})
	}
}

// deleteTaskReminder removes one reminder by id.
func deleteTaskReminder(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rid, err := intParam(r, "rid")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM task_reminders WHERE id = $1 AND user_id = $2", rid, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// formatReminderWhen renders the event instant in the user's local tz as a
// human phrase like "today at 5:00 PM" / "tomorrow at 9:00 AM" /
// "on Mon, Jun 2 at 5:00 PM". Falls back to defaultLoc (IST) when tz is unknown/bad.
func formatReminderWhen(at time.Time, tzName string) string {
	loc := defaultLoc
	if tzName != "" {
		if l, err := time.LoadLocation(tzName); err == nil {
			loc = l
		}
	}
	lt := at.In(loc)
	now := time.Now().In(loc)
	clock := lt.Format("3:04 PM")
	switch {
	case sameDay(lt, now):
		return "today at " + clock
	case sameDay(lt, now.AddDate(0, 0, 1)):
		return "tomorrow at " + clock
	default:
		return "on " + lt.Format("Mon, Jan 2") + " at " + clock
	}
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// userLocation loads the user's IANA tz into a *time.Location, falling back
// to defaultLoc (IST) when unset or unparseable. Shared by task handlers that
// derive a local due_date from a scheduled_at instant.
func userLocation(d *db.DB, uid string) *time.Location {
	var tz string
	d.QueryRow(`SELECT COALESCE(timezone,'') FROM users WHERE id = $1`, uid).Scan(&tz)
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return defaultLoc
}

// userNow is time.Now() in the user's timezone. Use it instead of bare
// time.Now() wherever a "today" date or month boundary is derived, so the
// date aligns with the user's clock rather than the server's (UTC on Cloud
// Run) — otherwise IST users between 00:00–05:30 land on the previous day.
func userNow(d *db.DB, uid string) time.Time {
	return time.Now().In(userLocation(d, uid))
}
