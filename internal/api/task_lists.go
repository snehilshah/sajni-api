package api

import (
	"net/http"
)

// task_lists are user-defined groupings of tasks (e.g. "Work",
// "Home"). Task rows reference one optionally; rows with NULL list_id
// land in the smart "Inbox" view on the frontend.
func registerTaskListRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("PUT /api/task-lists/reorder", reorderTaskLists(deps))

	mux.HandleFunc("GET /api/task-lists", listTaskLists(deps))
	mux.HandleFunc("POST /api/task-lists", createTaskList(deps))
	mux.HandleFunc("PUT /api/task-lists/{id}", updateTaskList(deps))
	mux.HandleFunc("DELETE /api/task-lists/{id}", deleteTaskList(deps))
}

type taskListRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	Icon      string `json:"icon"`
	SortOrder int    `json:"sort_order"`
	TaskCount int    `json:"task_count"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func listTaskLists(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`
			SELECT l.id, l.name, l.color, l.icon, l.sort_order,
			       COALESCE(c.cnt, 0)::int,
			       l.created_at, l.updated_at
			FROM task_lists l
			LEFT JOIN (
				SELECT list_id, COUNT(*) AS cnt FROM tasks
				WHERE user_id = $1 AND parent_task_id IS NULL AND status NOT IN ('done','scratched')
				GROUP BY list_id
			) c ON c.list_id = l.id
			WHERE l.user_id = $1
			ORDER BY l.sort_order ASC, l.id ASC`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []taskListRow{}
		for rows.Next() {
			var row taskListRow
			if err := rows.Scan(&row.ID, &row.Name, &row.Color, &row.Icon, &row.SortOrder, &row.TaskCount, &row.CreatedAt, &row.UpdatedAt); err == nil {
				out = append(out, row)
			}
		}
		writeJSON(w, 200, out)
	}
}

func createTaskList(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
			Icon  string `json:"icon"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Name == "" {
			errJSON(w, 400, "name required")
			return
		}
		if body.Color == "" {
			body.Color = "#2D5A4F"
		}
		if body.Icon == "" {
			body.Icon = "list"
		}
		var id int64
		err := d.QueryRow(`
			INSERT INTO task_lists (user_id, name, color, icon, sort_order)
			VALUES ($1, $2, $3, $4, COALESCE((SELECT MAX(sort_order)+1 FROM task_lists WHERE user_id=$1), 0))
			RETURNING id`, uid, body.Name, body.Color, body.Icon).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateTaskList(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Name  *string `json:"name"`
			Color *string `json:"color"`
			Icon  *string `json:"icon"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Name != nil {
			d.Exec("UPDATE task_lists SET name=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", *body.Name, id, uid)
		}
		if body.Color != nil {
			d.Exec("UPDATE task_lists SET color=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", *body.Color, id, uid)
		}
		if body.Icon != nil {
			d.Exec("UPDATE task_lists SET icon=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", *body.Icon, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteTaskList(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		// Tasks in this list become un-listed (Inbox) thanks to ON DELETE SET NULL.
		if _, err := d.Exec("DELETE FROM task_lists WHERE id=$1 AND user_id=$2", id, uid); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func reorderTaskLists(deps Deps) http.HandlerFunc {
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
			d.Exec("UPDATE task_lists SET sort_order=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3", i, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}
