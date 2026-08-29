package api

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

type shiftedTaskReminder struct {
	ID int64
	At time.Time
}

func rescheduleTaskDate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			TargetDate   string `json:"target_date"`
			ScheduleMode string `json:"schedule_mode"`
			ScheduledAt  string `json:"scheduled_at"`
		}
		if readJSON(r, &body) != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.ScheduleMode == "" {
			body.ScheduleMode = "preserve"
		}
		if body.ScheduleMode != "preserve" && body.ScheduleMode != "set" && body.ScheduleMode != "clear" {
			errJSON(w, 400, "invalid schedule_mode")
			return
		}
		loc := userLocation(deps.DB, uid)
		target, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(body.TargetDate), loc)
		if err != nil {
			errJSON(w, 400, "target_date must be YYYY-MM-DD")
			return
		}
		now := time.Now()
		today := now.In(loc)
		if target.Before(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)) {
			writeJSON(w, http.StatusConflict, map[string]any{"code": "schedule_would_be_past", "affected": []string{"due_date"}})
			return
		}

		tx, err := deps.DB.BeginTx(r.Context(), nil)
		if err != nil {
			internalError(w, r, "begin task reschedule", err)
			return
		}
		defer tx.Rollback()
		var oldDue sql.NullString
		var oldScheduled sql.NullTime
		var remind bool
		var status string
		if err := tx.QueryRowContext(r.Context(), `SELECT due_date::text,scheduled_at,remind,status FROM tasks WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, uid).
			Scan(&oldDue, &oldScheduled, &remind, &status); err != nil {
			errJSON(w, 404, "task not found")
			return
		}
		if !oldDue.Valid {
			errJSON(w, 400, "task has no day to reschedule")
			return
		}
		oldDate, err := time.ParseInLocation("2006-01-02", oldDue.String, loc)
		if err != nil {
			errJSON(w, 400, "task has invalid due date")
			return
		}
		delta := calendarDays(oldDate, target)
		var nextScheduled sql.NullTime
		switch body.ScheduleMode {
		case "preserve":
			if oldScheduled.Valid {
				nextScheduled = sql.NullTime{Time: shiftCalendarDays(oldScheduled.Time, delta, loc), Valid: true}
			}
		case "set":
			value, err := time.Parse(time.RFC3339, body.ScheduledAt)
			if err != nil || value.In(loc).Format("2006-01-02") != body.TargetDate {
				errJSON(w, 400, "scheduled_at must be on target_date")
				return
			}
			nextScheduled = sql.NullTime{Time: value, Valid: true}
		case "clear":
			remind = false
		}

		rows, err := tx.QueryContext(r.Context(), `SELECT id,remind_at FROM task_reminders WHERE task_id=$1 AND user_id=$2 FOR UPDATE`, id, uid)
		if err != nil {
			internalError(w, r, "lock task reminders", err)
			return
		}
		shifted := []shiftedTaskReminder{}
		for rows.Next() {
			var item shiftedTaskReminder
			var old time.Time
			if rows.Scan(&item.ID, &old) == nil {
				item.At = shiftCalendarDays(old, delta, loc)
				shifted = append(shifted, item)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			internalError(w, r, "read task reminders", err)
			return
		}
		rows.Close()
		affected := []string{}
		if nextScheduled.Valid && !nextScheduled.Time.After(now) {
			affected = append(affected, "scheduled_at")
		}
		for _, item := range shifted {
			if !item.At.After(now) {
				affected = append(affected, "task_reminders")
				break
			}
		}
		if len(affected) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"code": "schedule_would_be_past", "affected": affected})
			return
		}

		if _, err := tx.ExecContext(r.Context(), `UPDATE tasks SET due_date=$1,week_of=NULL,month_of=NULL,scheduled_at=$2,remind=$3,reminded_at=NULL,reminder_claimed_until=NULL,updated_at=NOW() WHERE id=$4 AND user_id=$5`, body.TargetDate, nextScheduled, remind, id, uid); err != nil {
			internalError(w, r, "update task schedule", err)
			return
		}
		for _, item := range shifted {
			if _, err := tx.ExecContext(r.Context(), `UPDATE task_reminders SET remind_at=$1,sent_at=NULL,claimed_until=NULL WHERE id=$2 AND user_id=$3`, item.At, item.ID, uid); err != nil {
				internalError(w, r, "update attached reminder", err)
				return
			}
		}
		if oldDue.String != body.TargetDate {
			logTaskEvent(tx, uid, id, "rescheduled", oldDue.String, body.TargetDate)
			if status != "done" && status != "scratched" && oldDue.String < today.Format("2006-01-02") {
				var count int
				if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM task_due_history WHERE user_id=$1 AND task_id=$2 AND due_date=$3`, uid, id, oldDue.String).Scan(&count); err != nil {
					internalError(w, r, "read task due history", err)
					return
				}
				if count == 0 {
					_, err = tx.ExecContext(r.Context(), `INSERT INTO task_due_history(user_id,task_id,due_date,outcome) VALUES($1,$2,$3,'rescheduled')`, uid, id, oldDue.String)
				} else {
					_, err = tx.ExecContext(r.Context(), `UPDATE task_due_history SET outcome='rescheduled' WHERE user_id=$1 AND task_id=$2 AND due_date=$3`, uid, id, oldDue.String)
				}
				if err != nil {
					internalError(w, r, "update task due history", err)
					return
				}
			}
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit task reschedule", err)
			return
		}
		if nextScheduled.Valid && remind {
			enqueueTaskReminderFromDB(r.Context(), deps.DB, deps.ReminderQueue, uid, id)
		}
		for _, item := range shifted {
			enqueueMultiReminder(r.Context(), deps.ReminderQueue, item.ID, item.At)
		}
		var scheduledAt any
		if nextScheduled.Valid {
			scheduledAt = nextScheduled.Time
		}
		writeJSON(w, 200, map[string]any{"id": id, "due_date": body.TargetDate, "scheduled_at": scheduledAt})
	}
}

func calendarDays(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}

func shiftCalendarDays(value time.Time, days int, loc *time.Location) time.Time {
	local := value.In(loc).AddDate(0, 0, days)
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), loc)
}
