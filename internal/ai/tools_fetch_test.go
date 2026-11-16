package ai

import (
	"context"
	"strconv"
	"testing"

	"sajni/internal/db"
	"sajni/internal/dbtest"
	"sajni/internal/storage"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/ai
func TestFetchReadsOneItemInFull(t *testing.T) {
	d, err := db.New(dbtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	uid, other := "01900000-0000-7000-8000-0000000000f6", "01900000-0000-7000-8000-0000000000f5"
	for _, u := range []string{uid, other} {
		d.Exec(`DELETE FROM users WHERE id = $1`, u)
		if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, $2, 'T')`, u, u+"@test"); err != nil {
			t.Fatal(err)
		}
		defer d.Exec(`DELETE FROM users WHERE id = $1`, u)
	}
	key := storage.UserKey(uid, "notes", "trip.md")
	store.Put(ctx, key, []byte("# Trip ideas\nGoa in March"), "text/markdown")
	var noteID, taskID int64
	d.QueryRow(`INSERT INTO notes (user_id, title, blob_key) VALUES ($1, 'Trip ideas', $2) RETURNING id`, uid, key).Scan(&noteID)
	d.QueryRow(`INSERT INTO tasks (user_id, title) VALUES ($1, 'Pack') RETURNING id`, uid).Scan(&taskID)
	d.Exec(`INSERT INTO tasks (user_id, title, parent_task_id) VALUES ($1, 'Sunscreen', $2)`, uid, taskID)

	got, _, err := runFetch(ctx, d, store, uid, "note:"+itoa(noteID))
	if err != nil {
		t.Fatal(err)
	}
	row := got.(map[string]any)
	if row["content"] != "# Trip ideas\nGoa in March" || row["title"] != "Trip ideas" || row["user_id"] != nil || row["blob_key"] != nil {
		t.Fatalf("note: %v", row)
	}
	got, _, err = runFetch(ctx, d, store, uid, "Task:"+itoa(taskID))
	if err != nil {
		t.Fatal(err)
	}
	if subs := got.(map[string]any)["subtasks"].([]any); len(subs) != 1 {
		t.Fatalf("subtasks: %v", subs)
	}
	if _, _, err := runFetch(ctx, d, store, other, "note:"+itoa(noteID)); err == nil {
		t.Fatal("another user read the note")
	}
	for _, bad := range []string{"note", "note:x", "secret:1", ""} {
		if _, _, err := runFetch(ctx, d, store, uid, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
