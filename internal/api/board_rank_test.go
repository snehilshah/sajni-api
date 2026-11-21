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

// Needs a scratch Postgres. Placing a card on the Board saves its rank and
// leaves updated_at, and so every list's order, alone.
func TestBoardRankIsArrangementNotAnEdit(t *testing.T) {
	d, err := db.New(dbtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000b0"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'board@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	mux := http.NewServeMux()
	registerTaskRoutes(mux, Deps{DB: d})
	call := func(method, path string, body any) (int, []byte) {
		var buf bytes.Buffer
		if body != nil {
			json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	var id int64
	d.QueryRow(`INSERT INTO tasks (user_id, title, updated_at) VALUES ($1, 'Pack', NOW() - INTERVAL '1 day') RETURNING id`, uid).Scan(&id)
	path := "/api/tasks/" + strconv.FormatInt(id, 10)

	var before string
	d.QueryRow(`SELECT updated_at::text FROM tasks WHERE id=$1`, id).Scan(&before)
	if code, b := call("PUT", path, map[string]any{"board_rank": -1.5e12}); code != 200 {
		t.Fatalf("PUT: %d %s", code, b)
	}
	var after string
	d.QueryRow(`SELECT updated_at::text FROM tasks WHERE id=$1`, id).Scan(&after)
	if after != before {
		t.Fatalf("updated_at moved: %s → %s", before, after)
	}
	_, b := call("GET", path, nil)
	var tr taskRow
	json.Unmarshal(b, &tr)
	if tr.BoardRank == nil || *tr.BoardRank != -1.5e12 {
		t.Fatalf("board_rank = %v", tr.BoardRank)
	}
	_, b = call("GET", "/api/tasks", nil)
	var list []taskRow
	json.Unmarshal(b, &list)
	if len(list) != 1 || list[0].BoardRank == nil || list[0].AttachmentCount != 0 {
		t.Fatalf("list: %s", b)
	}

	// The board shows how many images a task has.
	for i := 0; i < 2; i++ {
		if _, err := d.Exec(`INSERT INTO task_attachments (user_id, task_id, filename, content_type) VALUES ($1, $2, $3, 'image/png')`, uid, id, "a"+strconv.Itoa(i)+".png"); err != nil {
			t.Fatal(err)
		}
	}
	_, b = call("GET", "/api/tasks", nil)
	json.Unmarshal(b, &list)
	if list[0].AttachmentCount != 2 {
		t.Fatalf("attachment_count = %d", list[0].AttachmentCount)
	}
	_, b = call("GET", path, nil)
	json.Unmarshal(b, &tr)
	if tr.AttachmentCount != 2 {
		t.Fatalf("single attachment_count = %d", tr.AttachmentCount)
	}
}
