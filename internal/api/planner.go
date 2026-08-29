package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"sajni/internal/reminder"
)

type plannerReminderOccurrence struct {
	Key          string     `json:"key"`
	ReminderID   int64      `json:"reminder_id"`
	OccurrenceID *int64     `json:"occurrence_id,omitempty"`
	Sequence     int        `json:"sequence"`
	Message      string     `json:"message"`
	Notes        string     `json:"notes"`
	ScheduledAt  time.Time  `json:"scheduled_at"`
	FireAt       time.Time  `json:"fire_at"`
	Status       string     `json:"status"`
	Recurring    bool       `json:"recurring"`
	Projected    bool       `json:"projected"`
	DeliveredAt  *time.Time `json:"delivered_at,omitempty"`
	SkippedAt    *time.Time `json:"skipped_at,omitempty"`
}

type plannerReminderSeries struct {
	ID           int64
	Message      string
	Notes        string
	Timezone     string
	StartsAt     time.Time
	Rule         reminder.Rule
	OccurrenceID sql.NullInt64
	Sequence     sql.NullInt64
	ScheduledAt  sql.NullTime
	FireAt       sql.NullTime
}

func registerPlannerRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/planner", getPlanner(deps))
}

func getPlanner(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		loc := userLocation(deps.DB, uid)
		from, err := time.ParseInLocation("2006-01-02", queryParam(r, "from"), loc)
		if err != nil {
			errJSON(w, http.StatusBadRequest, "from must be YYYY-MM-DD")
			return
		}
		to, err := time.ParseInLocation("2006-01-02", queryParam(r, "to"), loc)
		if err != nil || to.Before(from) || calendarDays(from, to) > 41 {
			errJSON(w, http.StatusBadRequest, "to must be within 42 days of from")
			return
		}

		weekStart := mondayAt(from)
		weekEnd := mondayAt(to)
		monthStart := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, loc)
		monthEnd := time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, loc)
		tasks, err := plannerTasks(r, deps, uid, from, to, weekStart, weekEnd, monthStart, monthEnd)
		if err != nil {
			internalError(w, r, "planner tasks", err)
			return
		}
		occurrences, err := plannerReminders(r, deps, uid, from, to, loc)
		if err != nil {
			internalError(w, r, "planner reminders", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"timezone":             loc.String(),
			"from":                 from.Format("2006-01-02"),
			"to":                   to.Format("2006-01-02"),
			"tasks":                tasks,
			"reminder_occurrences": occurrences,
		})
	}
}

func mondayAt(value time.Time) time.Time {
	offset := (int(value.Weekday()) + 6) % 7
	return time.Date(value.Year(), value.Month(), value.Day()-offset, 0, 0, 0, 0, value.Location())
}

func plannerTasks(r *http.Request, deps Deps, uid string, from, to, weekStart, weekEnd, monthStart, monthEnd time.Time) ([]taskRow, error) {
	rows, err := deps.DB.QueryContext(r.Context(), `
		SELECT t.id,t.title,t.description,t.status,t.priority,t.color,
		       t.due_date::text,t.week_of::text,t.month_of::text,t.scheduled_at::text,
		       t.remind,t.reminded_at::text,COALESCE(t.notify_emails,'[]'::jsonb),
		       t.list_id,t.parent_task_id,t.blocked_by_task_id,blocker.title,blocker.status,
		       t.important,t.steps,COALESCE(t.sort_order,0),
		       COALESCE(c.cnt,0)::int,COALESCE(c.done,0)::int,t.created_at,t.updated_at
		FROM tasks t
		LEFT JOIN (
			SELECT parent_task_id,COUNT(*) cnt,COUNT(*) FILTER (WHERE status='done') done
			FROM tasks WHERE parent_task_id IS NOT NULL GROUP BY parent_task_id
		) c ON c.parent_task_id=t.id
		LEFT JOIN tasks blocker ON blocker.id=t.blocked_by_task_id AND blocker.user_id=t.user_id
		WHERE t.user_id=$1 AND t.status<>'scratched' AND (
			t.due_date BETWEEN $2 AND $3 OR t.week_of BETWEEN $4 AND $5 OR t.month_of BETWEEN $6 AND $7
		)
		ORDER BY COALESCE(t.due_date,t.week_of,t.month_of),t.scheduled_at NULLS LAST,t.sort_order,t.id`,
		uid, from.Format("2006-01-02"), to.Format("2006-01-02"),
		weekStart.Format("2006-01-02"), weekEnd.Format("2006-01-02"),
		monthStart.Format("2006-01-02"), monthEnd.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []taskRow{}
	for rows.Next() {
		var task taskRow
		var stepsRaw, emailsRaw []byte
		if err := rows.Scan(
			&task.ID, &task.Title, &task.Description, &task.Status, &task.Priority, &task.Color,
			&task.DueDate, &task.WeekOf, &task.MonthOf, &task.ScheduledAt,
			&task.Remind, &task.RemindedAt, &emailsRaw, &task.ListID, &task.ParentTaskID,
			&task.BlockedByTaskID, &task.BlockedByTaskTitle, &task.BlockedByTaskStatus,
			&task.Important, &stepsRaw, &task.SortOrder, &task.SubtaskCount, &task.SubtasksDone,
			&task.CreatedAt, &task.UpdatedAt,
		); err != nil {
			return nil, err
		}
		task.Steps = decodeSteps(stepsRaw)
		task.NotifyEmails = decodeEmails(emailsRaw)
		task.Tags = []string{}
		out = append(out, task)
	}
	return out, rows.Err()
}

func plannerReminders(r *http.Request, deps Deps, uid string, from, to time.Time, userLoc *time.Location) ([]plannerReminderOccurrence, error) {
	end := to.AddDate(0, 0, 1)
	actualRows, err := deps.DB.QueryContext(r.Context(), `
		SELECT o.id,o.reminder_id,o.sequence,r.message,r.notes,o.scheduled_at,o.fire_at,o.status,
		       o.delivered_at,o.skipped_at,COALESCE(r.recurrence->>'frequency','')<>''
		FROM reminder_occurrences o JOIN reminders r ON r.id=o.reminder_id AND r.user_id=o.user_id
		WHERE o.user_id=$1 AND o.status IN ('pending','delivered','skipped')
		  AND o.scheduled_at >= $2 AND o.scheduled_at < $3
		ORDER BY o.scheduled_at,o.id`, uid, from, end)
	if err != nil {
		return nil, err
	}
	out := []plannerReminderOccurrence{}
	seen := map[string]bool{}
	for actualRows.Next() {
		var item plannerReminderOccurrence
		var id int64
		if err := actualRows.Scan(&id, &item.ReminderID, &item.Sequence, &item.Message, &item.Notes,
			&item.ScheduledAt, &item.FireAt, &item.Status, &item.DeliveredAt, &item.SkippedAt, &item.Recurring); err != nil {
			actualRows.Close()
			return nil, err
		}
		item.OccurrenceID = &id
		item.Key = occurrenceKey(item.ReminderID, item.Sequence, item.ScheduledAt)
		seen[item.Key] = true
		out = append(out, item)
	}
	if err := actualRows.Err(); err != nil {
		actualRows.Close()
		return nil, err
	}
	actualRows.Close()

	seriesRows, err := deps.DB.QueryContext(r.Context(), `
		SELECT r.id,r.message,r.notes,r.timezone,r.starts_at,r.recurrence,
		       o.id,o.sequence,o.scheduled_at,o.fire_at
		FROM reminders r
		LEFT JOIN LATERAL (
			SELECT id,sequence,scheduled_at,fire_at FROM reminder_occurrences
			WHERE reminder_id=r.id AND user_id=r.user_id AND status='pending'
			ORDER BY scheduled_at LIMIT 1
		) o ON TRUE
		WHERE r.user_id=$1 AND r.active=TRUE`, uid)
	if err != nil {
		return nil, err
	}
	defer seriesRows.Close()
	for seriesRows.Next() {
		var series plannerReminderSeries
		var ruleRaw []byte
		if err := seriesRows.Scan(&series.ID, &series.Message, &series.Notes, &series.Timezone, &series.StartsAt,
			&ruleRaw, &series.OccurrenceID, &series.Sequence, &series.ScheduledAt, &series.FireAt); err != nil {
			return nil, err
		}
		if !series.ScheduledAt.Valid || json.Unmarshal(ruleRaw, &series.Rule) != nil || !series.Rule.Recurring() {
			continue
		}
		loc := userLoc
		if candidate, err := time.LoadLocation(series.Timezone); err == nil {
			loc = candidate
		}
		current := series.ScheduledAt.Time
		sequence := int(series.Sequence.Int64)
		for {
			next, ok := reminder.Next(series.StartsAt, current, sequence, series.Rule, loc)
			if !ok || !next.Before(end) {
				break
			}
			sequence++
			current = next
			if next.Before(from) {
				continue
			}
			key := occurrenceKey(series.ID, sequence, next)
			if seen[key] {
				continue
			}
			out = append(out, plannerReminderOccurrence{
				Key: key, ReminderID: series.ID, Sequence: sequence, Message: series.Message, Notes: series.Notes,
				ScheduledAt: next, FireAt: next, Status: "projected", Recurring: true, Projected: true,
			})
			seen[key] = true
		}
	}
	return out, seriesRows.Err()
}

func occurrenceKey(reminderID int64, sequence int, scheduled time.Time) string {
	return "reminder:" + strconv.FormatInt(reminderID, 10) + ":" + strconv.Itoa(sequence) + ":" + scheduled.UTC().Format(time.RFC3339)
}
