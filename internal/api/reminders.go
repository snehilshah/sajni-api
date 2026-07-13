package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
	"sajni/internal/push"
	"sajni/internal/reminderqueue"
)

// Reminders ride on tasks: a task with remind=TRUE and a scheduled_at gets
// one reminder at its scheduled time. The send is driven by Cloud Tasks
// (POST /internal/reminders/fire). The old sweep endpoint remains as a
// low-frequency safety net because Cloud Run scales to zero between requests.
// reminded_at is the idempotency gate.

// reminderGrace is the lower bound on how stale a reminder may be and still
// send. Without it, recovering from a multi-hour outage would blast every
// reminder whose window elapsed while we were down.
const reminderGrace = 30 * time.Minute
const reminderClaimLease = 5 * time.Minute

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

// RegisterReminderCronHandler mounts the internal reminder webhooks. Header
// X-Reminder-Cron must match REMINDER_CRON_SECRET; handlers 401 without it.
// /fire is called by Cloud Tasks, /run is the once-daily safety sweep.
func RegisterReminderCronHandler(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /internal/reminders/run", reminderCronHandler(deps))
	mux.HandleFunc("POST /internal/reminders/fire", reminderFireHandler(deps))
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

func reminderFireHandler(deps Deps) http.HandlerFunc {
	expected := os.Getenv("REMINDER_CRON_SECRET")
	return func(w http.ResponseWriter, r *http.Request) {
		if expected == "" || r.Header.Get("X-Reminder-Cron") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Kind string `json:"kind"`
			ID   int64  `json:"id"`
		}
		if err := readJSON(r, &body); err != nil || body.ID <= 0 {
			errJSON(w, 400, "invalid reminder fire payload")
			return
		}
		n, err := ProcessReminderFire(r.Context(), deps, body.Kind, body.ID)
		if err != nil {
			if _, ok := err.(errInvalidReminderKind); ok {
				errJSON(w, 400, err.Error())
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, 200, map[string]int{"sent": n})
	}
}

// ProcessReminderFire handles one Cloud Tasks delivery. Stale tasks are
// successful no-ops: Postgres remains the source of truth, and the idempotency
// stamps prevent duplicate sends.
func ProcessReminderFire(ctx context.Context, deps Deps, kind string, id int64) (int, error) {
	switch kind {
	case reminderqueue.KindTask:
		sent, err := sendSingleTaskReminder(ctx, deps, id)
		if err != nil {
			return 0, err
		}
		if sent {
			return 1, nil
		}
		return 0, nil
	case reminderqueue.KindMulti:
		sent, err := sendSingleMultiReminder(ctx, deps, id)
		if err != nil {
			return 0, err
		}
		if sent {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, errInvalidReminderKind(kind)
	}
}

type errInvalidReminderKind string

func (e errInvalidReminderKind) Error() string {
	return "invalid reminder kind: " + string(e)
}

// deliverTaskReminder ships one task nudge over the user's chosen channels
// (users.notify_channel: email | push | both). notifyPush enforces the push
// side; the owner email is skipped only when the user is push-only AND a push
// actually landed — otherwise email goes out as the fallback. Succeeds when at
// least one channel delivered, so the idempotency stamp still sets and the
// next tick doesn't re-nudge a user who already got one.
func deliverTaskReminder(ctx context.Context, deps Deps, uid, email, name, channel, title, whenLabel string, notifyEmails []string) error {
	pushed := notifyPush(ctx, deps, uid, push.Notification{
		Title: "Task reminder",
		Body:  title + " — " + whenLabel,
		Route: "/tasks",
	})

	// Custom recipients (e.g. a friend for a meet-up) get an email-only,
	// friendlier copy naming the owner. Best-effort and independent of the
	// owner's own delivery below — a guest failure never blocks the owner,
	// and the owner's channel choice never mutes their guests.
	if deps.Auth != nil {
		for _, addr := range notifyEmails {
			if err := deps.Auth.SendGuestTaskReminder(ctx, addr, name, title, whenLabel, "/tasks"); err != nil {
				log.Warn().Err(err).Str("to", addr).Msg("guest reminder email failed")
			}
		}
	}

	if deps.Auth == nil {
		if pushed {
			return nil
		}
		return errors.New("no delivery channel configured")
	}
	if !channelWantsEmail(channel, pushed) {
		return nil // push-only user, push delivered
	}
	if err := deps.Auth.SendTaskReminder(ctx, email, name, title, whenLabel, "/tasks"); err != nil {
		if pushed {
			log.Warn().Err(err).Str("user", uid).Msg("reminder email failed; push delivered")
			return nil
		}
		return err
	}
	return nil
}

// ProcessReminderCron emails every due, un-sent task reminder and stamps
// reminded_at. Idempotent: a row is only ever picked up once because the
// stamp drops it out of the window. A failed send leaves reminded_at NULL
// so the next tick retries.
func ProcessReminderCron(ctx context.Context, deps Deps) (int, error) {
	d := deps.DB
	if deps.Auth == nil && deps.Push == nil {
		return 0, nil // no delivery channel configured
	}

	// Window: scheduled_at has arrived, the event isn't stale beyond the grace
	// floor, the task is still open, and we haven't sent yet.
	rows, err := d.QueryContext(ctx, `
		WITH due AS (
			SELECT t.id
			  FROM tasks t
			  JOIN users u ON u.id = t.user_id
			 WHERE t.remind = TRUE
			   AND t.reminded_at IS NULL
			   AND (t.reminder_claimed_until IS NULL OR t.reminder_claimed_until < NOW())
			   AND t.status NOT IN ('done','scratched')
			   AND t.scheduled_at IS NOT NULL
			   AND t.scheduled_at <= NOW()
			   AND t.scheduled_at >= NOW() - make_interval(secs => $1)
			   AND u.deleted_at IS NULL
			 FOR UPDATE OF t SKIP LOCKED
		)
		UPDATE tasks t
		   SET reminder_claimed_until = NOW() + make_interval(secs => $2)
		  FROM due, users u
		 WHERE t.id = due.id AND u.id = t.user_id
		RETURNING t.id, t.title, t.scheduled_at, u.id, u.email, u.name, COALESCE(u.timezone,''),
		          COALESCE(u.notify_channel,'both'), COALESCE(t.notify_emails, '[]'::jsonb)`,
		int(reminderGrace.Seconds()), int(reminderClaimLease.Seconds()))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type due struct {
		id          int64
		title       string
		scheduledAt time.Time
		uid         string
		email, name string
		tz          string
		channel     string
		notify      []string
	}
	var pending []due
	for rows.Next() {
		var x due
		var emailsRaw []byte
		if err := rows.Scan(&x.id, &x.title, &x.scheduledAt, &x.uid, &x.email, &x.name, &x.tz, &x.channel, &emailsRaw); err != nil {
			continue
		}
		x.notify = decodeEmails(emailsRaw)
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
		if err := deliverTaskReminder(ctx, deps, x.uid, x.email, name, x.channel, x.title, whenLabel, x.notify); err != nil {
			if _, releaseErr := d.ExecContext(ctx, `UPDATE tasks SET reminder_claimed_until = NULL WHERE id = $1`, x.id); releaseErr != nil {
				log.Warn().Err(releaseErr).Int64("task", x.id).Msg("reminder claim release failed")
			}
			log.Warn().Err(err).Int64("task", x.id).Msg("reminder delivery failed")
			continue
		}
		if _, err := d.ExecContext(ctx, `UPDATE tasks SET reminded_at = NOW(), reminder_claimed_until = NULL WHERE id = $1`, x.id); err != nil {
			log.Warn().Err(err).Int64("task", x.id).Msg("reminder stamp failed")
			continue
		}
		sent++
	}

	// Second pass: explicit multi-reminders (task_reminders). These fire AT
	// their own remind_at (no lead) and can sit on any date, independent of
	// the task's own time. sent_at is the idempotency stamp.
	multiSent, err := processTaskReminders(ctx, deps)
	if err != nil {
		return sent, err
	}
	return sent + multiSent, nil
}

func sendSingleTaskReminder(ctx context.Context, deps Deps, id int64) (bool, error) {
	d := deps.DB
	if deps.Auth == nil && deps.Push == nil {
		return false, nil
	}
	type due struct {
		id          int64
		title       string
		scheduledAt time.Time
		uid         string
		email, name string
		tz          string
		channel     string
		notify      []string
	}
	var x due
	var emailsRaw []byte
	err := d.QueryRowContext(ctx, `
		WITH claimed AS (
			UPDATE tasks
			   SET reminder_claimed_until = NOW() + make_interval(secs => $3)
			 WHERE id = $1
			   AND remind = TRUE
			   AND reminded_at IS NULL
			   AND (reminder_claimed_until IS NULL OR reminder_claimed_until < NOW())
			   AND status NOT IN ('done','scratched')
			   AND scheduled_at IS NOT NULL
			   AND scheduled_at <= NOW()
			   AND scheduled_at >= NOW() - make_interval(secs => $2)
			 RETURNING *
		)
		SELECT t.id, t.title, t.scheduled_at, u.id, u.email, u.name, COALESCE(u.timezone,''),
		       COALESCE(u.notify_channel,'both'),
		       COALESCE(t.notify_emails, '[]'::jsonb)
		  FROM claimed t
		  JOIN users u ON u.id = t.user_id
		 WHERE u.deleted_at IS NULL`,
		id, int(reminderGrace.Seconds()), int(reminderClaimLease.Seconds())).
		Scan(&x.id, &x.title, &x.scheduledAt, &x.uid, &x.email, &x.name, &x.tz, &x.channel, &emailsRaw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		log.Warn().Err(err).Int64("task", id).Msg("single reminder query failed")
		return false, err
	}
	x.notify = decodeEmails(emailsRaw)
	name := x.name
	if name == "" {
		name = x.email
	}
	if err := deliverTaskReminder(ctx, deps, x.uid, x.email, name, x.channel, x.title, formatReminderWhen(x.scheduledAt, x.tz), x.notify); err != nil {
		_, _ = d.ExecContext(ctx, `UPDATE tasks SET reminder_claimed_until = NULL WHERE id = $1`, x.id)
		log.Warn().Err(err).Int64("task", x.id).Msg("single reminder delivery failed")
		return false, err
	}
	res, err := d.ExecContext(ctx, `UPDATE tasks SET reminded_at = NOW(), reminder_claimed_until = NULL WHERE id = $1 AND reminded_at IS NULL`, x.id)
	if err != nil {
		log.Warn().Err(err).Int64("task", x.id).Msg("single reminder stamp failed")
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// processTaskReminders emails every due, un-sent row in task_reminders and
// stamps sent_at. Same window/grace/idempotency model as the legacy path.
func processTaskReminders(ctx context.Context, deps Deps) (int, error) {
	d := deps.DB
	rows, err := d.QueryContext(ctx, `
		WITH due AS (
			SELECT r.id
			  FROM task_reminders r
			  JOIN tasks t ON t.id = r.task_id AND t.user_id = r.user_id
			  JOIN users u ON u.id = r.user_id
			 WHERE r.sent_at IS NULL
			   AND (r.claimed_until IS NULL OR r.claimed_until < NOW())
			   AND t.status NOT IN ('done','scratched')
			   AND r.remind_at <= NOW()
			   AND r.remind_at >= NOW() - make_interval(secs => $1)
			   AND u.deleted_at IS NULL
			 FOR UPDATE OF r SKIP LOCKED
		)
		UPDATE task_reminders r
		   SET claimed_until = NOW() + make_interval(secs => $2)
		  FROM due, tasks t, users u
		 WHERE r.id = due.id AND t.id = r.task_id AND t.user_id = r.user_id AND u.id = r.user_id
		RETURNING r.id, t.id, t.title, t.scheduled_at, r.remind_at,
		       u.id, u.email, u.name, COALESCE(u.timezone,''),
		       COALESCE(u.notify_channel,'both'),
		       COALESCE(t.notify_emails, '[]'::jsonb)`,
		int(reminderGrace.Seconds()), int(reminderClaimLease.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("claim task reminders: %w", err)
	}
	defer rows.Close()

	type due struct {
		rid, tid    int64
		title       string
		scheduledAt sql.NullTime
		remindAt    time.Time
		uid         string
		email, name string
		tz          string
		channel     string
		notify      []string
	}
	var pending []due
	for rows.Next() {
		var x due
		var emailsRaw []byte
		if err := rows.Scan(&x.rid, &x.tid, &x.title, &x.scheduledAt, &x.remindAt, &x.uid, &x.email, &x.name, &x.tz, &x.channel, &emailsRaw); err != nil {
			return 0, fmt.Errorf("scan task reminder: %w", err)
		}
		x.notify = decodeEmails(emailsRaw)
		pending = append(pending, x)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate task reminders: %w", err)
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
		if err := deliverTaskReminder(ctx, deps, x.uid, x.email, name, x.channel, x.title, formatReminderWhen(when, x.tz), x.notify); err != nil {
			_, _ = d.ExecContext(ctx, `UPDATE task_reminders SET claimed_until = NULL WHERE id = $1`, x.rid)
			log.Warn().Err(err).Int64("reminder", x.rid).Msg("task reminder delivery failed")
			continue
		}
		if _, err := d.ExecContext(ctx, `UPDATE task_reminders SET sent_at = NOW(), claimed_until = NULL WHERE id = $1`, x.rid); err != nil {
			log.Warn().Err(err).Int64("reminder", x.rid).Msg("task reminder stamp failed")
			continue
		}
		sent++
	}
	return sent, nil
}

func sendSingleMultiReminder(ctx context.Context, deps Deps, id int64) (bool, error) {
	d := deps.DB
	if deps.Auth == nil && deps.Push == nil {
		return false, nil
	}
	type due struct {
		rid, tid    int64
		title       string
		scheduledAt sql.NullTime
		remindAt    time.Time
		uid         string
		email, name string
		tz          string
		channel     string
		notify      []string
	}
	var x due
	var emailsRaw []byte
	err := d.QueryRowContext(ctx, `
		WITH claimed AS (
			UPDATE task_reminders
			   SET claimed_until = NOW() + make_interval(secs => $3)
			 WHERE id = $1
			   AND sent_at IS NULL
			   AND (claimed_until IS NULL OR claimed_until < NOW())
			   AND remind_at <= NOW()
			   AND remind_at >= NOW() - make_interval(secs => $2)
			 RETURNING *
		)
		SELECT r.id, t.id, t.title, t.scheduled_at, r.remind_at,
		       u.id, u.email, u.name, COALESCE(u.timezone,''),
		       COALESCE(u.notify_channel,'both'),
		       COALESCE(t.notify_emails, '[]'::jsonb)
		  FROM claimed r
		  JOIN tasks t ON t.id = r.task_id AND t.user_id = r.user_id
		  JOIN users u ON u.id = r.user_id
		 WHERE t.status NOT IN ('done','scratched')
		   AND u.deleted_at IS NULL`,
		id, int(reminderGrace.Seconds()), int(reminderClaimLease.Seconds())).
		Scan(&x.rid, &x.tid, &x.title, &x.scheduledAt, &x.remindAt, &x.uid, &x.email, &x.name, &x.tz, &x.channel, &emailsRaw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		log.Warn().Err(err).Int64("reminder", id).Msg("single task_reminder query failed")
		return false, err
	}
	x.notify = decodeEmails(emailsRaw)
	when := x.remindAt
	if x.scheduledAt.Valid {
		when = x.scheduledAt.Time
	}
	name := x.name
	if name == "" {
		name = x.email
	}
	if err := deliverTaskReminder(ctx, deps, x.uid, x.email, name, x.channel, x.title, formatReminderWhen(when, x.tz), x.notify); err != nil {
		_, _ = d.ExecContext(ctx, `UPDATE task_reminders SET claimed_until = NULL WHERE id = $1`, x.rid)
		log.Warn().Err(err).Int64("reminder", x.rid).Msg("single task reminder delivery failed")
		return false, err
	}
	res, err := d.ExecContext(ctx, `UPDATE task_reminders SET sent_at = NOW(), claimed_until = NULL WHERE id = $1 AND sent_at IS NULL`, x.rid)
	if err != nil {
		log.Warn().Err(err).Int64("reminder", x.rid).Msg("single task reminder stamp failed")
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func enqueueTaskReminderFromDB(ctx context.Context, d *db.DB, uid string, id int64) {
	var scheduledAt time.Time
	err := d.QueryRowContext(ctx, `
		SELECT scheduled_at
		  FROM tasks
		 WHERE id = $1
		   AND user_id = $2
		   AND remind = TRUE
		   AND reminded_at IS NULL
		   AND scheduled_at IS NOT NULL
		   AND status NOT IN ('done','scratched')`,
		id, uid).Scan(&scheduledAt)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		log.Warn().Err(err).Int64("task", id).Msg("reminder enqueue lookup failed")
		return
	}
	if err := reminderqueue.EnqueueTask(ctx, id, scheduledAt); err != nil {
		log.Warn().Err(err).Int64("task", id).Msg("reminder cloud task enqueue failed")
	}
}

func enqueueMultiReminder(ctx context.Context, id int64, remindAt time.Time) {
	if err := reminderqueue.EnqueueMulti(ctx, id, remindAt); err != nil {
		log.Warn().Err(err).Int64("reminder", id).Msg("task_reminder cloud task enqueue failed")
	}
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
		var remindAt time.Time
		if err := d.QueryRow(
			`INSERT INTO task_reminders (user_id, task_id, remind_at) VALUES ($1, $2, $3) RETURNING id, remind_at`,
			uid, id, body.RemindAt,
		).Scan(&rid, &remindAt); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		enqueueMultiReminder(r.Context(), rid, remindAt)
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
