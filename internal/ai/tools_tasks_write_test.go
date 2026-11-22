package ai

import (
	"context"
	"strings"
	"testing"

	"sajni/internal/db"
	"sajni/internal/dbtest"
	"sajni/internal/storage"
)

// The AI's task writes refuse loops and unknown ids, and a delete takes the
// task's tags and backlinks (and its subtasks') with it, as in the app.
// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/ai
func TestAITaskWritesGuardAndCleanUp(t *testing.T) {
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
	uid := "01900000-0000-7000-8000-0000000000e7"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, $2, 'T')`, uid, uid+"@test"); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)

	s := &Service{db: d, store: store}
	s.tools = s.buildTools()
	call := func(name string, args map[string]any) error {
		_, _, err := s.dispatch(ctx, uid, name, args)
		return err
	}

	var a, b, c int64
	d.QueryRow(`INSERT INTO tasks (user_id, title) VALUES ($1, 'Trip') RETURNING id`, uid).Scan(&a)
	d.QueryRow(`INSERT INTO tasks (user_id, title, parent_task_id) VALUES ($1, 'Pack', $2) RETURNING id`, uid, a).Scan(&b)
	d.QueryRow(`INSERT INTO tasks (user_id, title, parent_task_id) VALUES ($1, 'Sunscreen', $2) RETURNING id`, uid, b).Scan(&c)

	// Loops: under itself, under its child, under its grandchild.
	for _, pid := range []int64{a, b, c} {
		if err := call("update_task", map[string]any{"id": float64(a), "parent_task_id": float64(pid)}); err == nil || !strings.Contains(err.Error(), "under itself") {
			t.Errorf("parent %d: err = %v, want a loop refusal", pid, err)
		}
	}
	var parent *int64
	d.QueryRow(`SELECT parent_task_id FROM tasks WHERE id=$1`, a).Scan(&parent)
	if parent != nil {
		t.Fatalf("Trip got parent %d", *parent)
	}
	// Moving a leaf under another branch is still fine.
	var other int64
	d.QueryRow(`INSERT INTO tasks (user_id, title) VALUES ($1, 'Errands') RETURNING id`, uid).Scan(&other)
	if err := call("update_task", map[string]any{"id": float64(c), "parent_task_id": float64(other)}); err != nil {
		t.Fatalf("plain re-parent: %v", err)
	}
	d.Exec(`UPDATE tasks SET parent_task_id=$1 WHERE id=$2`, b, c)

	// Unknown ids are errors, not success.
	const missing = 99999999
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"mark_task_done", map[string]any{"id": float64(missing)}},
		{"complete_task", map[string]any{"id": float64(missing)}},
		{"set_task_important", map[string]any{"id": float64(missing), "important": true}},
		{"delete_task", map[string]any{"id": float64(missing)}},
	} {
		if err := call(tc.tool, tc.args); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s on a missing task: err = %v", tc.tool, err)
		}
	}
	var events int
	d.QueryRow(`SELECT count(*) FROM task_events WHERE user_id=$1 AND task_id=$2`, uid, missing).Scan(&events)
	if events != 0 {
		t.Errorf("missing task got %d events", events)
	}

	// Delete: tags and backlinks of the tree go too; other tasks' stay.
	for _, id := range []int64{a, b, c, other} {
		if err := db.SyncTags(ctx, d, uid, "task", id, "#trip [[Packing list]]"); err != nil {
			t.Fatal(err)
		}
		if err := db.SyncBacklinks(ctx, d, uid, "task", id, "#trip [[Packing list]]"); err != nil {
			t.Fatal(err)
		}
	}
	if err := call("delete_task", map[string]any{"id": float64(a)}); err != nil {
		t.Fatal(err)
	}
	var tags, links int
	d.QueryRow(`SELECT count(*) FROM tags WHERE user_id=$1 AND entity_type='task' AND entity_id = ANY($2)`, uid, []int64{a, b, c}).Scan(&tags)
	d.QueryRow(`SELECT count(*) FROM backlinks WHERE user_id=$1 AND source_type='task' AND source_id = ANY($2)`, uid, []int64{a, b, c}).Scan(&links)
	if tags != 0 || links != 0 {
		t.Errorf("after delete: %d tags, %d backlinks left", tags, links)
	}
	d.QueryRow(`SELECT count(*) FROM tags WHERE user_id=$1 AND entity_type='task' AND entity_id=$2`, uid, other).Scan(&tags)
	d.QueryRow(`SELECT count(*) FROM backlinks WHERE user_id=$1 AND source_type='task' AND source_id=$2`, uid, other).Scan(&links)
	if tags != 1 || links != 1 {
		t.Errorf("Errands lost its tag/backlink: %d, %d", tags, links)
	}
}
