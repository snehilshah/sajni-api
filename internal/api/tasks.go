package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"sajni/internal/db"
)

// maxNotifyEmails caps the extra reminder recipients per task. Outbound mail
// to arbitrary addresses is an abuse surface; for a single-owner PKMS a small
// cap + per-address validation is the pragmatic guard.
const maxNotifyEmails = 3

// sanitizeNotifyEmails trims, RFC-validates, lowercases and de-dups the custom
// reminder recipients, capping at maxNotifyEmails. Invalid entries are dropped.
func sanitizeNotifyEmails(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, e := range in {
		addr, err := mail.ParseAddress(strings.TrimSpace(e))
		if err != nil {
			continue
		}
		a := strings.ToLower(addr.Address)
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
		if len(out) >= maxNotifyEmails {
			break
		}
	}
	return out
}

// decodeEmails tolerates malformed JSONB by returning an empty slice.
func decodeEmails(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// weekBounds returns this week's Monday and Sunday (YYYY-MM-DD) in the user's
// tz. Weeks are Monday-anchored to match the journal weekly view; week_of on a
// week task always stores the Monday.
func weekBounds(d *db.DB, uid string) (monday, sunday string) {
	now := userNow(d, uid)
	offset := (int(now.Weekday()) + 6) % 7 // days since Monday (Sun=6)
	mon := now.AddDate(0, 0, -offset)
	return mon.Format("2006-01-02"), mon.AddDate(0, 0, 6).Format("2006-01-02")
}

func registerTaskRoutes(mux *http.ServeMux, deps Deps) {
	// Specific paths must register before /{id}.
	mux.HandleFunc("GET /api/tasks/missed", listMissedTasks(deps))
	mux.HandleFunc("PUT /api/tasks/reorder", reorderTasks(deps))
	mux.HandleFunc("GET /api/tasks/{id}/history", getTaskHistory(deps))
	mux.HandleFunc("GET /api/tasks/{id}/events", getTaskEvents(deps))
	mux.HandleFunc("GET /api/tasks/{id}/subtasks", listSubtasks(deps))
	mux.HandleFunc("GET /api/tasks/{id}/reminders", listTaskReminders(deps))
	mux.HandleFunc("POST /api/tasks/{id}/reminders", addTaskReminder(deps))
	mux.HandleFunc("DELETE /api/tasks/{id}/reminders/{rid}", deleteTaskReminder(deps))

	mux.HandleFunc("GET /api/tasks", listTasks(deps))
	mux.HandleFunc("POST /api/tasks", createTask(deps))
	mux.HandleFunc("GET /api/tasks/{id}", getTask(deps))
	mux.HandleFunc("PUT /api/tasks/{id}", updateTask(deps))
	mux.HandleFunc("DELETE /api/tasks/{id}", deleteTask(deps))
}

// getTask returns a single task by id. Used by the global task detail
// popup so any chip/row click can open the same dialog without first
// pulling the whole list and filtering.
func getTask(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var t taskRow
		var stepsRaw, emailsRaw []byte
		err = d.QueryRow(`
			SELECT t.id, t.title, t.description, t.status, t.priority,
			       t.due_date::text, t.week_of::text, t.scheduled_at::text,
			       t.remind, t.reminded_at::text, COALESCE(t.notify_emails, '[]'::jsonb),
			       t.list_id, t.parent_task_id, t.important, t.steps,
			       COALESCE(t.sort_order, 0),
			       COALESCE(c.cnt, 0)::int, COALESCE(c.done, 0)::int,
			       t.created_at, t.updated_at
			FROM tasks t
			LEFT JOIN (
				SELECT parent_task_id,
				       COUNT(*) AS cnt,
				       COUNT(*) FILTER (WHERE status = 'done') AS done
				FROM tasks
				WHERE parent_task_id IS NOT NULL
				GROUP BY parent_task_id
			) c ON c.parent_task_id = t.id
			WHERE t.user_id = $1 AND t.id = $2`, uid, id).Scan(
			&t.ID, &t.Title, &t.Description, &t.Status, &t.Priority,
			&t.DueDate, &t.WeekOf, &t.ScheduledAt,
			&t.Remind, &t.RemindedAt, &emailsRaw,
			&t.ListID, &t.ParentTaskID, &t.Important, &stepsRaw,
			&t.SortOrder, &t.SubtaskCount, &t.SubtasksDone,
			&t.CreatedAt, &t.UpdatedAt,
		)
		if err != nil {
			errJSON(w, 404, "not found")
			return
		}
		t.Steps = decodeSteps(stepsRaw)
		t.NotifyEmails = decodeEmails(emailsRaw)
		tagRows, err := d.Query("SELECT tag FROM tags WHERE user_id = $1 AND entity_type = 'task' AND entity_id = $2", uid, t.ID)
		if err == nil {
			for tagRows.Next() {
				var tag string
				tagRows.Scan(&tag)
				t.Tags = append(t.Tags, tag)
			}
			tagRows.Close()
		}
		if t.Tags == nil {
			t.Tags = []string{}
		}
		writeJSON(w, 200, t)
	}
}

// taskRow is the wire shape returned to clients. Tasks v2 adds list_id,
// parent_task_id, important, steps, sort_order, and a precomputed
// subtask_count so the list-view can show the nested-tasks hint
// without a separate request per row.
type taskRow struct {
	ID           int64    `json:"id"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Status       string   `json:"status"`
	Priority     string   `json:"priority"`
	Tags         []string `json:"tags"`
	DueDate      *string  `json:"due_date"`
	WeekOf       *string  `json:"week_of"`
	ScheduledAt  *string  `json:"scheduled_at"`
	Remind       bool     `json:"remind"`
	RemindedAt   *string  `json:"reminded_at"`
	NotifyEmails []string `json:"notify_emails"`
	ListID       *int64   `json:"list_id"`
	ParentTaskID *int64   `json:"parent_task_id"`
	Important    bool     `json:"important"`
	Steps        []Step   `json:"steps"`
	SortOrder    int      `json:"sort_order"`
	SubtaskCount int      `json:"subtask_count"`
	SubtasksDone int      `json:"subtasks_done"`
	// Subtasks are embedded (brief shape) so the list view can render the
	// nested children instantly on expand — no per-row /subtasks round-trip.
	Subtasks  []subtaskBrief `json:"subtasks"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// subtaskBrief is the lightweight child shape embedded in a task list row
// and returned by the dedicated /subtasks endpoint.
type subtaskBrief struct {
	ID           int64   `json:"id"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	Priority     string  `json:"priority"`
	DueDate      *string `json:"due_date"`
	Important    bool    `json:"important"`
	ParentTaskID *int64  `json:"parent_task_id"`
	SortOrder    int     `json:"sort_order"`
}

// Step is one item on a task's inline checklist (Microsoft-Todo style).
type Step struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

func listTasks(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())

		args := []any{uid}
		clauses := []string{"t.user_id = $1"}
		ph := 2

		// Smart list shortcut: my_day | important | planned | scheduled |
		// missed | all | inbox. If absent, fall back to the list/status filters.
		// "active" excludes both done and scratched — a scratched task is
		// abandoned, so it must drop out of every act-on-it view (and the
		// reminder cron, see reminders.go) the same way a done task does.
		// Dates are the USER's local day (userNow), not the server's UTC
		// CURRENT_DATE — otherwise an IST user between 00:00–05:30 sees the
		// wrong "today"/"overdue" set.
		const active = "t.status NOT IN ('done','scratched')"
		today := userNow(d, uid).Format("2006-01-02")
		switch queryParam(r, "smart") {
		case "my_day":
			clauses = append(clauses, "t.due_date = $"+itoa(ph), active)
			args = append(args, today)
			ph++
		case "important":
			clauses = append(clauses, "t.important = TRUE", active)
		case "planned":
			clauses = append(clauses, "t.due_date IS NOT NULL", active)
		case "scheduled":
			clauses = append(clauses, "t.scheduled_at IS NOT NULL", active)
		case "week":
			// Week-scoped tasks for the current (Monday-anchored) week.
			mon, _ := weekBounds(d, uid)
			clauses = append(clauses, "t.week_of = $"+itoa(ph), active)
			args = append(args, mon)
			ph++
		case "missed":
			// Every still-open task whose day is already past — accumulates,
			// not just yesterday. Drives the Missed smart list + the reschedule
			// banner. A week task is missed once its whole week has elapsed
			// (week_of before this Monday). Scratched/done excluded by `active`.
			mon, _ := weekBounds(d, uid)
			clauses = append(clauses, "(t.due_date < $"+itoa(ph)+" OR t.week_of < $"+itoa(ph+1)+")", active)
			args = append(args, today, mon)
			ph += 2
		case "inbox":
			clauses = append(clauses, "t.list_id IS NULL")
		case "all":
			// no extra filter (returns done + scratched too; the client
			// buckets them into collapsible groups)
		}

		// Explicit list filter: numeric id, "none" for tasks without a list.
		if list := queryParam(r, "list"); list != "" {
			if list == "none" {
				clauses = append(clauses, "t.list_id IS NULL")
			} else {
				clauses = append(clauses, "t.list_id = $"+itoa(ph))
				args = append(args, list)
				ph++
			}
		}

		// parent: numeric id (children of a task) or "null" (top-level only).
		// Default: top-level only when no smart/list filter is in play.
		switch p := queryParam(r, "parent"); p {
		case "":
			if queryParam(r, "smart") == "" && queryParam(r, "list") == "" {
				clauses = append(clauses, "t.parent_task_id IS NULL")
			}
		case "null":
			clauses = append(clauses, "t.parent_task_id IS NULL")
		default:
			clauses = append(clauses, "t.parent_task_id = $"+itoa(ph))
			args = append(args, p)
			ph++
		}

		if s := queryParam(r, "status"); s != "" {
			clauses = append(clauses, "t.status = $"+itoa(ph))
			args = append(args, s)
			ph++
		}
		if dd := queryParam(r, "due_date"); dd != "" {
			clauses = append(clauses, "t.due_date = $"+itoa(ph))
			args = append(args, dd)
			ph++
		}
		// week_of: week tasks for a specific (Monday-anchored) week — drives the
		// journal weekly view, which can page through past/future weeks.
		if wo := queryParam(r, "week_of"); wo != "" {
			clauses = append(clauses, "t.week_of = $"+itoa(ph))
			args = append(args, wo)
			ph++
		}
		if cd := queryParam(r, "completed_date"); cd != "" {
			clauses = append(clauses, "t.status = 'done' AND t.updated_at::date = $"+itoa(ph))
			args = append(args, cd)
			ph++
		}

		// My Day is a derived view (not manually drag-ordered), so it leads
		// with priority then the day's clock time. Explicit lists keep their
		// sort_order drag-ordering.
		orderBy := `ORDER BY t.sort_order ASC,
			         CASE t.priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END,
			         t.created_at DESC`
		if queryParam(r, "smart") == "my_day" {
			orderBy = `ORDER BY CASE t.priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END,
			         t.scheduled_at ASC NULLS LAST,
			         t.created_at DESC`
		}
		// Missed leads with the oldest overdue day so the longest-ignored
		// task is first to reschedule.
		if queryParam(r, "smart") == "missed" {
			orderBy = `ORDER BY t.due_date ASC,
			         CASE t.priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END,
			         t.created_at DESC`
		}

		q := `
			SELECT t.id, t.title, t.description, t.status, t.priority,
			       t.due_date::text, t.week_of::text, t.scheduled_at::text,
			       t.remind, t.reminded_at::text, COALESCE(t.notify_emails, '[]'::jsonb),
			       t.list_id, t.parent_task_id, t.important, t.steps,
			       COALESCE(t.sort_order, 0),
			       COALESCE(c.cnt, 0)::int, COALESCE(c.done, 0)::int,
			       t.created_at, t.updated_at
			FROM tasks t
			LEFT JOIN (
				SELECT parent_task_id,
				       COUNT(*) AS cnt,
				       COUNT(*) FILTER (WHERE status = 'done') AS done
				FROM tasks
				WHERE parent_task_id IS NOT NULL
				GROUP BY parent_task_id
			) c ON c.parent_task_id = t.id
			WHERE ` + strings.Join(clauses, " AND ") + `
			` + orderBy

		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		var tasks []taskRow
		for rows.Next() {
			var t taskRow
			var stepsRaw, emailsRaw []byte
			if err := rows.Scan(
				&t.ID, &t.Title, &t.Description, &t.Status, &t.Priority,
				&t.DueDate, &t.WeekOf, &t.ScheduledAt,
				&t.Remind, &t.RemindedAt, &emailsRaw,
				&t.ListID, &t.ParentTaskID, &t.Important, &stepsRaw,
				&t.SortOrder, &t.SubtaskCount, &t.SubtasksDone,
				&t.CreatedAt, &t.UpdatedAt,
			); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			t.Steps = decodeSteps(stepsRaw)
			t.NotifyEmails = decodeEmails(emailsRaw)
			tagRows, err := d.Query("SELECT tag FROM tags WHERE user_id = $1 AND entity_type = 'task' AND entity_id = $2", uid, t.ID)
			if err == nil {
				for tagRows.Next() {
					var tag string
					tagRows.Scan(&tag)
					t.Tags = append(t.Tags, tag)
				}
				tagRows.Close()
			}
			if t.Tags == nil {
				t.Tags = []string{}
			}
			tasks = append(tasks, t)
		}
		if tasks == nil {
			tasks = []taskRow{}
		}

		// Prefetch every returned task's children in ONE query and attach them,
		// so the client can expand subtasks instantly (no per-row request).
		if len(tasks) > 0 {
			idx := make(map[int64]int, len(tasks))
			ph2 := []string{}
			cargs := []any{uid}
			for i := range tasks {
				idx[tasks[i].ID] = i
				cargs = append(cargs, tasks[i].ID)
				ph2 = append(ph2, "$"+itoa(len(cargs)))
			}
			crows, cerr := d.Query(`
				SELECT id, title, status, priority, due_date::text,
				       important, parent_task_id, COALESCE(sort_order, 0)
				FROM tasks
				WHERE user_id = $1 AND parent_task_id IN (`+strings.Join(ph2, ",")+`)
				ORDER BY sort_order ASC, created_at ASC`, cargs...)
			if cerr == nil {
				for crows.Next() {
					var s subtaskBrief
					if crows.Scan(&s.ID, &s.Title, &s.Status, &s.Priority, &s.DueDate,
						&s.Important, &s.ParentTaskID, &s.SortOrder) == nil && s.ParentTaskID != nil {
						if i, ok := idx[*s.ParentTaskID]; ok {
							tasks[i].Subtasks = append(tasks[i].Subtasks, s)
						}
					}
				}
				crows.Close()
			}
		}

		writeJSON(w, 200, tasks)
	}
}

func listSubtasks(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		rows, err := d.Query(`
			SELECT id, title, status, priority, due_date::text,
			       important, COALESCE(sort_order, 0)
			FROM tasks
			WHERE user_id = $1 AND parent_task_id = $2
			ORDER BY sort_order ASC, created_at ASC`, uid, id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Sub struct {
			ID        int64   `json:"id"`
			Title     string  `json:"title"`
			Status    string  `json:"status"`
			Priority  string  `json:"priority"`
			DueDate   *string `json:"due_date"`
			Important bool    `json:"important"`
			SortOrder int     `json:"sort_order"`
		}
		out := []Sub{}
		for rows.Next() {
			var s Sub
			rows.Scan(&s.ID, &s.Title, &s.Status, &s.Priority, &s.DueDate, &s.Important, &s.SortOrder)
			out = append(out, s)
		}
		writeJSON(w, 200, out)
	}
}

func createTask(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Title        string   `json:"title"`
			Description  string   `json:"description"`
			Priority     string   `json:"priority"`
			Status       string   `json:"status"`
			DueDate      *string  `json:"due_date"`
			WeekOf       *string  `json:"week_of"`
			ScheduledAt  *string  `json:"scheduled_at"`
			Remind       bool     `json:"remind"`
			NotifyEmails []string `json:"notify_emails"`
			ListID       *int64   `json:"list_id"`
			ParentTaskID *int64   `json:"parent_task_id"`
			Important    bool     `json:"important"`
			Steps        []Step   `json:"steps"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Priority == "" {
			body.Priority = "medium"
		}
		if body.Status == "" {
			body.Status = "todo"
		}
		stepsJSON, _ := json.Marshal(normalizeSteps(body.Steps))

		// Validate list ownership; ignore silently if it doesn't belong to user.
		if body.ListID != nil {
			var ok int
			d.QueryRow("SELECT 1 FROM task_lists WHERE id=$1 AND user_id=$2", *body.ListID, uid).Scan(&ok)
			if ok != 1 {
				body.ListID = nil
			}
		}
		if body.ParentTaskID != nil {
			var ok int
			d.QueryRow("SELECT 1 FROM tasks WHERE id=$1 AND user_id=$2", *body.ParentTaskID, uid).Scan(&ok)
			if ok != 1 {
				body.ParentTaskID = nil
			}
		}

		// Empty-string due_date from the form is "no date", not "1970-01-01".
		// Postgres rejects '' as a DATE — coerce to NULL up front.
		var dueArg any
		if body.DueDate != nil && strings.TrimSpace(*body.DueDate) != "" {
			dueArg = *body.DueDate
		}

		// week_of marks a week-scoped task (no specific day). Same ''→NULL guard.
		var weekArg any
		if body.WeekOf != nil && strings.TrimSpace(*body.WeekOf) != "" {
			weekArg = *body.WeekOf
		}

		notifyJSON, _ := json.Marshal(sanitizeNotifyEmails(body.NotifyEmails))

		// scheduled_at is the event instant (powers time chips + reminders).
		var schedArg any
		if body.ScheduledAt != nil && strings.TrimSpace(*body.ScheduledAt) != "" {
			schedArg = *body.ScheduledAt
			// Keep due_date (the all-day bucket the smart-lists use) in sync
			// with the scheduled day in the user's local tz when the client
			// didn't send one explicitly.
			if dueArg == nil {
				if t, err := time.Parse(time.RFC3339, *body.ScheduledAt); err == nil {
					dueArg = t.In(userLocation(d, uid)).Format("2006-01-02")
				}
			}
		}

		// Compute the next sort_order in a separate query. Inlining it
		// inside the INSERT made Postgres unable to infer the type of the
		// nullable bigint placeholders, throwing 500 on empty payloads.
		var nextSort int
		listScope, parentScope := int64(0), int64(0)
		if body.ListID != nil {
			listScope = *body.ListID
		}
		if body.ParentTaskID != nil {
			parentScope = *body.ParentTaskID
		}
		d.QueryRow(`
			SELECT COALESCE(MAX(sort_order)+1, 0)
			  FROM tasks
			 WHERE user_id = $1
			   AND COALESCE(list_id, 0)        = $2
			   AND COALESCE(parent_task_id, 0) = $3`,
			uid, listScope, parentScope,
		).Scan(&nextSort)

		var id int64
		err := d.QueryRow(`
			INSERT INTO tasks (user_id, title, description, priority, status, due_date, week_of, scheduled_at, remind,
			                   notify_emails, list_id, parent_task_id, important, steps, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13, $14::jsonb, $15)
			RETURNING id`,
			uid, body.Title, body.Description, body.Priority, body.Status, dueArg, weekArg, schedArg, body.Remind,
			string(notifyJSON), body.ListID, body.ParentTaskID, body.Important, stepsJSON, nextSort,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}

		contentForTags := body.Title + " " + body.Description
		syncTags(d, uid, "task", id, contentForTags)
		syncBacklinks(d, uid, "task", id, contentForTags)

		logTaskEvent(d, uid, id, "created", "", body.Title)
		if body.Remind {
			enqueueTaskReminderFromDB(r.Context(), d, uid, id)
		}

		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateTask(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		// Pointers everywhere so we can distinguish "set null" from "leave alone":
		// any field with a non-nil pointer is updated. due_date/list_id/parent
		// also accept the JSON literal null to clear the column.
		var body struct {
			Title          *string   `json:"title"`
			Description    *string   `json:"description"`
			Status         *string   `json:"status"`
			Priority       *string   `json:"priority"`
			DueDate        *string   `json:"due_date"`
			WeekOf         *string   `json:"week_of"`
			ScheduledAt    *string   `json:"scheduled_at"`
			Remind         *bool     `json:"remind"`
			NotifyEmails   *[]string `json:"notify_emails"`
			ListID         *int64    `json:"list_id"`
			ParentTaskID   *int64    `json:"parent_task_id"`
			Important      *bool     `json:"important"`
			Steps          *[]Step   `json:"steps"`
			SortOrder      *int      `json:"sort_order"`
			ClearList      bool      `json:"clear_list"`
			ClearParent    bool      `json:"clear_parent"`
			ClearScheduled bool      `json:"clear_scheduled"`
			ClearDue       bool      `json:"clear_due"`
			ClearWeek      bool      `json:"clear_week"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		reminderTimingChanged := body.ScheduledAt != nil || body.ClearScheduled || body.Remind != nil

		// Lifecycle snapshot: due-date misses + audit-trail diffing.
		var (
			currentDueDate *string
			currentStatus  string
			currentTitle   string
			currentListID  sql.NullInt64
		)
		d.QueryRow("SELECT due_date::text, status, title, list_id FROM tasks WHERE id = $1 AND user_id = $2", id, uid).
			Scan(&currentDueDate, &currentStatus, &currentTitle, &currentListID)

		var contentForTags string
		if body.Title != nil {
			d.Exec("UPDATE tasks SET title = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Title, id, uid)
			contentForTags += *body.Title + " "
			if *body.Title != currentTitle {
				logTaskEvent(d, uid, id, "title", currentTitle, *body.Title)
			}
		} else {
			var t string
			d.QueryRow("SELECT title FROM tasks WHERE id = $1 AND user_id = $2", id, uid).Scan(&t)
			contentForTags += t + " "
		}
		if body.Description != nil {
			d.Exec("UPDATE tasks SET description = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Description, id, uid)
			contentForTags += *body.Description
		} else {
			var dDesc string
			d.QueryRow("SELECT description FROM tasks WHERE id = $1 AND user_id = $2", id, uid).Scan(&dDesc)
			contentForTags += dDesc
		}
		if body.Title != nil || body.Description != nil {
			syncTags(d, uid, "task", id, contentForTags)
			syncBacklinks(d, uid, "task", id, contentForTags)
		}

		if body.Status != nil {
			d.Exec("UPDATE tasks SET status = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Status, id, uid)
			if *body.Status != currentStatus {
				logTaskEvent(d, uid, id, "status", currentStatus, *body.Status)
			}
		}
		if body.Priority != nil {
			d.Exec("UPDATE tasks SET priority = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Priority, id, uid)
		}
		if body.DueDate != nil {
			oldDate := ""
			if currentDueDate != nil {
				oldDate = *currentDueDate
			}
			newDate := *body.DueDate
			if oldDate != "" && oldDate != newDate && currentStatus != "done" && currentStatus != "scratched" {
				// If the old due date was already past (the task was missed) and
				// we're moving to a new date, the lifecycle entry for that date is
				// a "rescheduled", not a bare "missed" — record it that way so the
				// Lifecycle list reads "rescheduled" instead of "missed".
				today := userNow(d, uid).Format("2006-01-02")
				outcome, rescheduled := rescheduleOutcome(oldDate, newDate, today)
				var cnt int
				d.QueryRow(
					"SELECT COUNT(*) FROM task_due_history WHERE user_id = $1 AND task_id = $2 AND due_date = $3",
					uid, id, oldDate,
				).Scan(&cnt)
				if cnt == 0 {
					d.Exec(
						"INSERT INTO task_due_history (user_id, task_id, due_date, outcome) VALUES ($1, $2, $3, $4)",
						uid, id, oldDate, outcome,
					)
				} else if rescheduled {
					// Promote a prior bare "missed" row for this date to "rescheduled".
					d.Exec(
						"UPDATE task_due_history SET outcome = 'rescheduled' WHERE user_id = $1 AND task_id = $2 AND due_date = $3",
						uid, id, oldDate,
					)
				}
				if rescheduled {
					logTaskEvent(d, uid, id, "rescheduled", oldDate, newDate)
				}
			}
			d.Exec("UPDATE tasks SET due_date = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", newDate, id, uid)
		}
		if body.ListID != nil || body.ClearList {
			var v sql.NullInt64
			if body.ListID != nil && !body.ClearList {
				// Validate list ownership.
				var ok int
				d.QueryRow("SELECT 1 FROM task_lists WHERE id=$1 AND user_id=$2", *body.ListID, uid).Scan(&ok)
				if ok == 1 {
					v = sql.NullInt64{Int64: *body.ListID, Valid: true}
				}
			}
			d.Exec("UPDATE tasks SET list_id=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", v, id, uid)
			if v.Int64 != currentListID.Int64 || v.Valid != currentListID.Valid {
				logTaskEvent(d, uid, id, "list", listLabel(d, uid, currentListID), listLabel(d, uid, v))
			}
		}
		if body.ParentTaskID != nil || body.ClearParent {
			var v sql.NullInt64
			if body.ParentTaskID != nil && !body.ClearParent && *body.ParentTaskID != id {
				var ok int
				d.QueryRow("SELECT 1 FROM tasks WHERE id=$1 AND user_id=$2", *body.ParentTaskID, uid).Scan(&ok)
				if ok == 1 {
					v = sql.NullInt64{Int64: *body.ParentTaskID, Valid: true}
				}
			}
			d.Exec("UPDATE tasks SET parent_task_id=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", v, id, uid)
		}
		// scheduled_at: set or clear the event time. Either change clears
		// reminded_at so the reminder re-arms (an edited time should fire
		// again). When a time is set and the client didn't override due_date,
		// keep due_date aligned to the new local day for the smart-lists.
		if body.ScheduledAt != nil || body.ClearScheduled {
			var schedArg any
			if body.ScheduledAt != nil && !body.ClearScheduled && strings.TrimSpace(*body.ScheduledAt) != "" {
				schedArg = *body.ScheduledAt
			}
			d.Exec("UPDATE tasks SET scheduled_at=$1, reminded_at=NULL, updated_at=NOW() WHERE id=$2 AND user_id=$3", schedArg, id, uid)
			if s, ok := schedArg.(string); ok && body.DueDate == nil {
				if t, err := time.Parse(time.RFC3339, s); err == nil {
					d.Exec("UPDATE tasks SET due_date=$1 WHERE id=$2 AND user_id=$3", t.In(userLocation(d, uid)).Format("2006-01-02"), id, uid)
				}
			}
		}
		if body.Remind != nil {
			// Toggling remind re-arms too (turning it back on should re-send).
			d.Exec("UPDATE tasks SET remind=$1, reminded_at=NULL, updated_at=NOW() WHERE id=$2 AND user_id=$3", *body.Remind, id, uid)
		}
		if body.Important != nil {
			d.Exec("UPDATE tasks SET important=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", *body.Important, id, uid)
		}
		// week_of: set/clear the Monday anchor of a week task. Switching a task
		// between "day" and "week" scope is the form sending week_of + clear_due
		// (→ week) or due_date + clear_week (→ day) so only one is ever set.
		if body.WeekOf != nil || body.ClearWeek {
			var v any
			if body.WeekOf != nil && !body.ClearWeek && strings.TrimSpace(*body.WeekOf) != "" {
				v = *body.WeekOf
			}
			d.Exec("UPDATE tasks SET week_of=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", v, id, uid)
		}
		if body.ClearDue {
			d.Exec("UPDATE tasks SET due_date=NULL, updated_at=NOW() WHERE id=$1 AND user_id=$2", id, uid)
		}
		if body.NotifyEmails != nil {
			b, _ := json.Marshal(sanitizeNotifyEmails(*body.NotifyEmails))
			d.Exec("UPDATE tasks SET notify_emails=$1::jsonb, updated_at=NOW() WHERE id=$2 AND user_id=$3", string(b), id, uid)
		}
		if body.Steps != nil {
			stepsJSON, _ := json.Marshal(normalizeSteps(*body.Steps))
			d.Exec("UPDATE tasks SET steps=$1::jsonb, updated_at=NOW() WHERE id=$2 AND user_id=$3", stepsJSON, id, uid)
		}
		if body.SortOrder != nil {
			d.Exec("UPDATE tasks SET sort_order=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", *body.SortOrder, id, uid)
		}
		if reminderTimingChanged {
			enqueueTaskReminderFromDB(r.Context(), d, uid, id)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteTask(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		// Cleanup tags/backlinks for this row and its descendants too —
		// schema cascades the rows themselves on parent_task_id, but tags
		// live in a side table keyed by entity_id.
		descRows, _ := d.Query(`
			WITH RECURSIVE tree AS (
				SELECT id FROM tasks WHERE id=$1 AND user_id=$2
				UNION ALL
				SELECT t.id FROM tasks t JOIN tree ON t.parent_task_id = tree.id
			) SELECT id FROM tree`, id, uid)
		var ids []int64
		if descRows != nil {
			for descRows.Next() {
				var x int64
				if err := descRows.Scan(&x); err == nil {
					ids = append(ids, x)
				}
			}
			descRows.Close()
		}
		for _, x := range ids {
			d.Exec("DELETE FROM tags WHERE user_id=$1 AND entity_type='task' AND entity_id=$2", uid, x)
			d.Exec("DELETE FROM backlinks WHERE user_id=$1 AND source_type='task' AND source_id=$2", uid, x)
		}
		d.Exec("DELETE FROM tasks WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func reorderTasks(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			IDs []int64 `json:"ids"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		for i, id := range body.IDs {
			d.Exec("UPDATE tasks SET sort_order=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", i, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func listMissedTasks(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		date := queryParam(r, "date")
		if date == "" {
			errJSON(w, 400, "missing date")
			return
		}

		type MissedTask struct {
			ID             int64   `json:"id"`
			Title          string  `json:"title"`
			Status         string  `json:"status"`
			Priority       string  `json:"priority"`
			MissedDate     string  `json:"missed_date"`
			CurrentDueDate *string `json:"current_due_date"`
			Source         string  `json:"source"`
		}

		var out []MissedTask
		seen := map[int64]struct{}{}

		rows, err := d.Query(
			`SELECT t.id, t.title, t.status, t.priority, t.due_date::text, h.due_date::text
			 FROM task_due_history h
			 JOIN tasks t ON t.id = h.task_id AND t.user_id = h.user_id
			 WHERE h.user_id = $1 AND h.due_date = $2 AND h.outcome = 'missed'`,
			uid, date,
		)
		if err == nil {
			for rows.Next() {
				var m MissedTask
				rows.Scan(&m.ID, &m.Title, &m.Status, &m.Priority, &m.CurrentDueDate, &m.MissedDate)
				m.Source = "rescheduled"
				seen[m.ID] = struct{}{}
				out = append(out, m)
			}
			rows.Close()
		}

		rows2, err := d.Query(
			`SELECT id, title, status, priority, due_date::text
			 FROM tasks
			 WHERE user_id = $1 AND due_date = $2 AND status NOT IN ('done','scratched') AND $2::date < CURRENT_DATE`,
			uid, date,
		)
		if err == nil {
			for rows2.Next() {
				var m MissedTask
				rows2.Scan(&m.ID, &m.Title, &m.Status, &m.Priority, &m.CurrentDueDate)
				if _, dup := seen[m.ID]; dup {
					continue
				}
				m.MissedDate = date
				m.Source = "overdue"
				out = append(out, m)
			}
			rows2.Close()
		}

		if out == nil {
			out = []MissedTask{}
		}
		writeJSON(w, 200, out)
	}
}

func getTaskHistory(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		rows, err := d.Query(
			"SELECT due_date::text, outcome, recorded_at FROM task_due_history WHERE user_id = $1 AND task_id = $2 ORDER BY recorded_at ASC",
			uid, id,
		)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Entry struct {
			DueDate    string `json:"due_date"`
			Outcome    string `json:"outcome"`
			RecordedAt string `json:"recorded_at"`
		}
		var entries []Entry
		for rows.Next() {
			var e Entry
			rows.Scan(&e.DueDate, &e.Outcome, &e.RecordedAt)
			entries = append(entries, e)
		}
		if entries == nil {
			entries = []Entry{}
		}
		writeJSON(w, 200, entries)
	}
}

// logTaskEvent appends one audit-trail row. Best-effort: a failure here
// must never block the mutation it records, so the error is swallowed.
func logTaskEvent(d *db.DB, uid string, taskID int64, kind, from, to string) {
	d.Exec(`INSERT INTO task_events (user_id, task_id, kind, from_val, to_val)
	        VALUES ($1,$2,$3,$4,$5)`, uid, taskID, kind, from, to)
}

// listLabel resolves a nullable list_id to a human label for the audit
// trail — "Inbox" when unfiled or the list can't be found.
func listLabel(d *db.DB, uid string, v sql.NullInt64) string {
	if !v.Valid {
		return "Inbox"
	}
	var name string
	d.QueryRow("SELECT name FROM task_lists WHERE id=$1 AND user_id=$2", v.Int64, uid).Scan(&name)
	if name == "" {
		return "Inbox"
	}
	return name
}

// getTaskEvents returns the audit timeline (oldest-first) for one task —
// the GitHub-style feed the detail drawer renders.
func getTaskEvents(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		rows, err := d.Query(
			`SELECT kind, from_val, to_val, created_at FROM task_events
			 WHERE user_id=$1 AND task_id=$2 ORDER BY created_at ASC, id ASC`, uid, id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Event struct {
			Kind      string `json:"kind"`
			From      string `json:"from"`
			To        string `json:"to"`
			CreatedAt string `json:"created_at"`
		}
		out := []Event{}
		for rows.Next() {
			var e Event
			rows.Scan(&e.Kind, &e.From, &e.To, &e.CreatedAt)
			out = append(out, e)
		}
		writeJSON(w, 200, out)
	}
}

// rescheduleOutcome classifies what to record for a task's PREVIOUS due date
// when its due date changes while the task is still open. Moving off an
// already-past day onto a real new day is a "rescheduled" (the user salvaged
// it); any other change leaves the lapsed day standing as "missed". today is
// the user's local YYYY-MM-DD. Pure, so the Missed/Lifecycle semantics are
// unit-testable without a DB. The SQL "active" filter (status NOT IN
// ('done','scratched')) is the row-level mirror of this same intent.
func rescheduleOutcome(oldDate, newDate, today string) (outcome string, rescheduled bool) {
	if oldDate != "" && oldDate < today && strings.TrimSpace(newDate) != "" {
		return "rescheduled", true
	}
	return "missed", false
}

// decodeSteps tolerates malformed JSONB by returning an empty slice.
func decodeSteps(raw []byte) []Step {
	if len(raw) == 0 {
		return []Step{}
	}
	var out []Step
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []Step{}
	}
	return out
}

// normalizeSteps assigns a stable id to any step missing one and
// drops empties, so the array we persist is always well-formed.
func normalizeSteps(in []Step) []Step {
	out := make([]Step, 0, len(in))
	for _, s := range in {
		s.Text = strings.TrimSpace(s.Text)
		if s.Text == "" {
			continue
		}
		if s.ID == "" {
			s.ID = "s_" + time.Now().Format("150405.000000000") + "_" + itoa(len(out))
		}
		out = append(out, s)
	}
	return out
}
