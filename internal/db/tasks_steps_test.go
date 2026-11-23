package db_test

import (
	"context"
	"testing"

	"sajni/internal/db"
	"sajni/internal/dbtest"
)

// Unticked steps keep a task from being marked done, as open subtasks do.
func TestOpenStepCount(t *testing.T) {
	dsn := dbtest.URL(t)
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	uid := "01900000-0000-7000-8000-0000000000b8"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'steps@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var id int64
	if err := d.QueryRow(`INSERT INTO tasks (user_id, title, steps) VALUES ($1, 'Ship', $2::jsonb) RETURNING id`, uid,
		`[{"id":"a","text":"Draft","done":true},{"id":"b","text":"Test","done":false},{"id":"c","text":"Deploy"}]`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if n := db.OpenStepCount(ctx, d, uid, id); n != 2 {
		t.Fatalf("open steps = %d, want 2", n)
	}
	if got := db.OpenStepsMessage(2); got != "Tick its 2 open steps first" {
		t.Fatalf("message = %q", got)
	}
	d.Exec(`UPDATE tasks SET steps = '[]' WHERE id = $1`, id)
	if n := db.OpenStepCount(ctx, d, uid, id); n != 0 {
		t.Fatalf("no steps counted %d open", n)
	}
}
