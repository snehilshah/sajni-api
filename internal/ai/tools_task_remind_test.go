package ai

import (
	"context"
	"testing"
	"time"

	"sajni/internal/db"
	"sajni/internal/dbtest"
	"sajni/internal/reminderqueue"
)

// A task given a time reminds by default, as picking a time does in the apps.
// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/ai
func TestTaskTimeTurnsReminderOn(t *testing.T) {
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
	uid := "01900000-0000-7000-8000-0000000000a7"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'remind@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var q reminderqueue.Queue
	at := time.Now().Add(48 * time.Hour).Truncate(time.Minute).Format(time.RFC3339)
	day := at[:10]

	create := func(args map[string]any) int64 {
		t.Helper()
		data, _, err := createTaskTool(ctx, d, q, uid, args)
		if err != nil {
			t.Fatal(err)
		}
		return data.(map[string]any)["id"].(int64)
	}
	remind := func(id int64) (on bool) {
		t.Helper()
		if err := d.QueryRow(`SELECT remind FROM tasks WHERE id = $1`, id).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return
	}

	if id := create(map[string]any{"title": "Timed", "scheduled_at": at}); !remind(id) {
		t.Fatal("a task created with a time has no reminder")
	}
	if id := create(map[string]any{"title": "Planning only", "scheduled_at": at, "remind": false}); remind(id) {
		t.Fatal("remind=false was ignored")
	}
	if id := create(map[string]any{"title": "Dated", "due_date": day, "remind": true}); remind(id) {
		t.Fatal("a task with no time was set to remind")
	}

	// Rescheduling: a first time turns it on; moving an existing time keeps the choice.
	undated := create(map[string]any{"title": "Later", "due_date": day})
	if _, _, err := rescheduleTaskTool(ctx, d, q, uid, map[string]any{"id": float64(undated), "scheduled_at": at}); err != nil {
		t.Fatal(err)
	}
	if !remind(undated) {
		t.Fatal("a first time given by reschedule didn't turn the reminder on")
	}
	quiet := create(map[string]any{"title": "Quiet", "scheduled_at": at, "remind": false})
	moved := time.Now().Add(49 * time.Hour).Truncate(time.Minute).Format(time.RFC3339)
	if moved[:10] != day {
		moved = at
	}
	if _, _, err := rescheduleTaskTool(ctx, d, q, uid, map[string]any{"id": float64(quiet), "scheduled_at": moved}); err != nil {
		t.Fatal(err)
	}
	if remind(quiet) {
		t.Fatal("moving a time switched an off reminder back on")
	}
}
