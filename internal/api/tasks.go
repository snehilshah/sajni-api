package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func registerTaskRoutes(mux *http.ServeMux, deps Deps) {
	// Specific paths must register before /{id}.
	mux.HandleFunc("GET /api/tasks/missed", listMissedTasks(deps))
	mux.HandleFunc("PUT /api/tasks/reorder", reorderTasks(deps))
	mux.HandleFunc("GET /api/tasks/{id}/history", getTaskHistory(deps))
	mux.HandleFunc("GET /api/tasks/{id}/subtasks", listSubtasks(deps))

	mux.HandleFunc("GET /api/tasks", listTasks(deps))
	mux.HandleFunc("POST /api/tasks", createTask(deps))
	mux.HandleFunc("PUT /api/tasks/{id}", updateTask(deps))
	mux.HandleFunc("DELETE /api/tasks/{id}", deleteTask(deps))
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
	ScheduledAt  *string  `json:"scheduled_at"`
	ListID       *int64   `json:"list_id"`
	ParentTaskID *int64   `json:"parent_task_id"`
	Important    bool     `json:"important"`
	Steps        []Step   `json:"steps"`
	SortOrder    int      `json:"sort_order"`
	SubtaskCount int      `json:"subtask_count"`
	SubtasksDone int      `json:"subtasks_done"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
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

		// Smart list shortcut: my_day | important | planned | all | inbox.
		// If absent, fall back to the existing list/status filters.
		switch queryParam(r, "smart") {
		case "my_day":
			clauses = append(clauses, "t.due_date = CURRENT_DATE", "t.status != 'done'")
		case "important":
			clauses = append(clauses, "t.important = TRUE", "t.status != 'done'")
		case "planned":
			clauses = append(clauses, "t.due_date IS NOT NULL", "t.status != 'done'")
		case "inbox":
			clauses = append(clauses, "t.list_id IS NULL")
		case "all":
			// no extra filter
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
		if cd := queryParam(r, "completed_date"); cd != "" {
			clauses = append(clauses, "t.status = 'done' AND t.updated_at::date = $"+itoa(ph))
			args = append(args, cd)
			ph++
		}

		q := `
			SELECT t.id, t.title, t.description, t.status, t.priority,
			       t.due_date::text, t.scheduled_at::text,
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
			ORDER BY t.sort_order ASC,
			         CASE t.priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END,
			         t.created_at DESC`

		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		var tasks []taskRow
		for rows.Next() {
			var t taskRow
			var stepsRaw []byte
			if err := rows.Scan(
				&t.ID, &t.Title, &t.Description, &t.Status, &t.Priority,
				&t.DueDate, &t.ScheduledAt,
				&t.ListID, &t.ParentTaskID, &t.Important, &stepsRaw,
				&t.SortOrder, &t.SubtaskCount, &t.SubtasksDone,
				&t.CreatedAt, &t.UpdatedAt,
			); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			t.Steps = decodeSteps(stepsRaw)
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
			Title        string  `json:"title"`
			Description  string  `json:"description"`
			Priority     string  `json:"priority"`
			Status       string  `json:"status"`
			DueDate      *string `json:"due_date"`
			ListID       *int64  `json:"list_id"`
			ParentTaskID *int64  `json:"parent_task_id"`
			Important    bool    `json:"important"`
			Steps        []Step  `json:"steps"`
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
			INSERT INTO tasks (user_id, title, description, priority, status, due_date,
			                   list_id, parent_task_id, important, steps, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11)
			RETURNING id`,
			uid, body.Title, body.Description, body.Priority, body.Status, dueArg,
			body.ListID, body.ParentTaskID, body.Important, stepsJSON, nextSort,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}

		contentForTags := body.Title + " " + body.Description
		syncTags(d, uid, "task", id, contentForTags)
		syncBacklinks(d, uid, "task", id, contentForTags)

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
			Title        *string `json:"title"`
			Description  *string `json:"description"`
			Status       *string `json:"status"`
			Priority     *string `json:"priority"`
			DueDate      *string `json:"due_date"`
			ListID       *int64  `json:"list_id"`
			ParentTaskID *int64  `json:"parent_task_id"`
			Important    *bool   `json:"important"`
			Steps        *[]Step `json:"steps"`
			SortOrder    *int    `json:"sort_order"`
			ClearList    bool    `json:"clear_list"`
			ClearParent  bool    `json:"clear_parent"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}

		// Lifecycle snapshot for due-date misses.
		var (
			currentDueDate *string
			currentStatus  string
		)
		d.QueryRow("SELECT due_date::text, status FROM tasks WHERE id = $1 AND user_id = $2", id, uid).Scan(&currentDueDate, &currentStatus)

		var contentForTags string
		if body.Title != nil {
			d.Exec("UPDATE tasks SET title = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Title, id, uid)
			contentForTags += *body.Title + " "
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
			if oldDate != "" && oldDate != newDate && currentStatus != "done" {
				var cnt int
				d.QueryRow(
					"SELECT COUNT(*) FROM task_due_history WHERE user_id = $1 AND task_id = $2 AND due_date = $3 AND outcome = 'missed'",
					uid, id, oldDate,
				).Scan(&cnt)
				if cnt == 0 {
					d.Exec(
						"INSERT INTO task_due_history (user_id, task_id, due_date, outcome) VALUES ($1, $2, $3, 'missed')",
						uid, id, oldDate,
					)
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
		if body.Important != nil {
			d.Exec("UPDATE tasks SET important=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", *body.Important, id, uid)
		}
		if body.Steps != nil {
			stepsJSON, _ := json.Marshal(normalizeSteps(*body.Steps))
			d.Exec("UPDATE tasks SET steps=$1::jsonb, updated_at=NOW() WHERE id=$2 AND user_id=$3", stepsJSON, id, uid)
		}
		if body.SortOrder != nil {
			d.Exec("UPDATE tasks SET sort_order=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", *body.SortOrder, id, uid)
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
			 WHERE user_id = $1 AND due_date = $2 AND status != 'done' AND $2::date < CURRENT_DATE`,
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
