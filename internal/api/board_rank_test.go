package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// Needs a scratch Postgres. Done holds the last 7 days in every view,
// open-only ones included (a card dropped on Done stays in sight); older
// done tasks leave every done=recent list for the Archive.
func TestDoneIsSevenDaysThenArchive(t *testing.T) {
	d, err := db.New(dbtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000b1"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name, timezone) VALUES ($1, 'board2@test', 'T', 'Asia/Kolkata')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	mux := http.NewServeMux()
	registerTaskRoutes(mux, Deps{DB: d})
	get := func(path string, into any) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		json.Unmarshal(rec.Body.Bytes(), into)
	}
	titles := func(q string) map[string]bool {
		var list []taskRow
		get("/api/tasks?"+q, &list)
		out := map[string]bool{}
		for _, x := range list {
			out[x.Title] = true
		}
		return out
	}
	var listID int64
	d.QueryRow(`INSERT INTO task_lists (user_id, name) VALUES ($1, 'Office') RETURNING id`, uid).Scan(&listID)
	today := "(now() AT TIME ZONE 'Asia/Kolkata')::date"
	d.Exec(`INSERT INTO tasks (user_id, title, status, due_date, list_id) VALUES
		($1, 'open', 'todo', `+today+`, $2),
		($1, 'done now', 'todo', `+today+`, $2),
		($1, 'done 3d', 'done', `+today+`, $2),
		($1, 'done 9d', 'done', `+today+`, $2),
		($1, 'done 30d', 'done', `+today+`, NULL)`, uid, listID)
	// The trigger stamps done_at when a task becomes done.
	d.Exec(`UPDATE tasks SET status = 'done' WHERE user_id = $1 AND title = 'done now'`, uid)
	var stamped bool
	d.QueryRow(`SELECT done_at > now() - interval '1 minute' FROM tasks WHERE user_id = $1 AND title = 'done now'`, uid).Scan(&stamped)
	if !stamped {
		t.Fatal("done_at not stamped on done")
	}
	d.Exec(`UPDATE tasks SET done_at = now() - interval '3 days' WHERE user_id = $1 AND title = 'done 3d'`, uid)
	d.Exec(`UPDATE tasks SET done_at = now() - interval '9 days' WHERE user_id = $1 AND title = 'done 9d'`, uid)
	d.Exec(`UPDATE tasks SET done_at = now() - interval '30 days' WHERE user_id = $1 AND title = 'done 30d'`, uid)

	// Old clients (no param) see what they always did.
	if got := titles("smart=my_day"); !got["open"] || got["done now"] {
		t.Fatalf("my_day: %v", got)
	}
	if got := titles("smart=all"); !got["done 30d"] {
		t.Fatalf("all: %v", got)
	}
	for _, q := range []string{"smart=my_day&done=recent", "smart=planned&done=recent", "smart=all&done=recent"} {
		got := titles(q)
		if !got["open"] || !got["done now"] || !got["done 3d"] || got["done 9d"] || got["done 30d"] {
			t.Fatalf("%s: %v", q, got)
		}
	}
	if got := titles("list=" + strconv.FormatInt(listID, 10) + "&parent=null&done=recent"); !got["done 3d"] || got["done 9d"] {
		t.Fatalf("list: %v", got)
	}

	type page struct {
		Items      []taskRow `json:"items"`
		Count      int       `json:"count"`
		HasMore    bool      `json:"has_more"`
		NextCursor *string   `json:"next_cursor"`
	}
	var p page
	get("/api/tasks/archive?limit=0", &p)
	if p.Count != 2 || len(p.Items) != 0 {
		t.Fatalf("count: %+v", p)
	}
	get("/api/tasks/archive?list="+strconv.FormatInt(listID, 10)+"&limit=0", &p)
	if p.Count != 1 {
		t.Fatalf("list count: %+v", p)
	}
	// Newest done first, a page at a time.
	get("/api/tasks/archive?limit=1", &p)
	if len(p.Items) != 1 || p.Items[0].Title != "done 9d" || !p.HasMore || p.NextCursor == nil {
		t.Fatalf("page 1: %+v", p)
	}
	var p2 page
	get("/api/tasks/archive?limit=1&cursor="+url.QueryEscape(*p.NextCursor), &p2)
	if len(p2.Items) != 1 || p2.Items[0].Title != "done 30d" || p2.HasMore {
		t.Fatalf("page 2: %+v", p2)
	}

	// Reopened, it leaves the Archive and its done_at clears.
	d.Exec(`UPDATE tasks SET status = 'todo' WHERE user_id = $1 AND title = 'done 9d'`, uid)
	var cleared bool
	d.QueryRow(`SELECT done_at IS NULL FROM tasks WHERE user_id = $1 AND title = 'done 9d'`, uid).Scan(&cleared)
	get("/api/tasks/archive?limit=0", &p)
	if !cleared || p.Count != 1 {
		t.Fatalf("reopen: cleared=%v %+v", cleared, p)
	}
}
