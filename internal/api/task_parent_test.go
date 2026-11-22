package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/dbtest"
)

// Needs a scratch Postgres. A task can't be moved under one of its own
// subtasks: the loop would make every tree walk run until the timeout.
func TestTaskCantGoUnderItsOwnSubtask(t *testing.T) {
	d, err := db.New(dbtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000b7"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'parent@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	mux := http.NewServeMux()
	registerTaskRoutes(mux, Deps{DB: d})
	put := func(id int64, body any) int {
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest("PUT", "/api/tasks/"+strconv.FormatInt(id, 10), &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	var a, b, c int64
	d.QueryRow(`INSERT INTO tasks (user_id, title) VALUES ($1, 'Trip') RETURNING id`, uid).Scan(&a)
	d.QueryRow(`INSERT INTO tasks (user_id, title, parent_task_id) VALUES ($1, 'Pack', $2) RETURNING id`, uid, a).Scan(&b)
	d.QueryRow(`INSERT INTO tasks (user_id, title, parent_task_id) VALUES ($1, 'Sunscreen', $2) RETURNING id`, uid, b).Scan(&c)

	if code := put(a, map[string]any{"parent_task_id": c, "title": "Trip!"}); code != 400 {
		t.Fatalf("under its grandchild: %d, want 400", code)
	}
	var parent *int64
	var title string
	d.QueryRow(`SELECT parent_task_id, title FROM tasks WHERE id=$1`, a).Scan(&parent, &title)
	if parent != nil || title != "Trip" {
		t.Fatalf("refused save still wrote: parent %v, title %q", parent, title)
	}
	if code := put(c, map[string]any{"parent_task_id": a}); code != 200 {
		t.Fatalf("plain re-parent: %d", code)
	}
}
