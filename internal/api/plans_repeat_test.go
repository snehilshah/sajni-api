package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/taskrepeat"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run PlansAndRepeats
func TestPlansAndRepeats(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-00000000000a"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name, timezone) VALUES ($1, 'plans@test', 'T', 'Asia/Kolkata')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)

	mux := http.NewServeMux()
	deps := Deps{DB: d}
	registerTaskRoutes(mux, deps)
	registerPlanRoutes(mux, deps)
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
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := time.Now().In(loc)
	day := func(offset int) string { return today.AddDate(0, 0, offset).Format("2006-01-02") }
	task := func(id int64) taskRow {
		_, b := call("GET", "/api/tasks/"+strconv.FormatInt(id, 10), nil)
		var tr taskRow
		json.Unmarshal(b, &tr)
		return tr
	}
	create := func(body map[string]any) int64 {
		code, b := call("POST", "/api/tasks", body)
		if code != 201 {
			t.Fatalf("create: %d %s", code, b)
		}
		var out struct{ ID int64 }
		json.Unmarshal(b, &out)
		return out.ID
	}
	daily := map[string]any{"frequency": "daily"}

	// A repeat needs a date.
	if code, _ := call("POST", "/api/tasks", map[string]any{"title": "x", "recurrence": daily}); code != 400 {
		t.Fatalf("undated repeat: %d", code)
	}

	// Done yesterday: reopens today, steps unticked.
	done := create(map[string]any{"title": "Check board", "due_date": day(-1), "recurrence": daily,
		"steps": []map[string]any{{"text": "Open board", "done": true}}})
	call("PUT", "/api/tasks/"+strconv.FormatInt(done, 10), map[string]any{"status": "done"})
	// Open yesterday: a miss, moves to today.
	open := create(map[string]any{"title": "Inbox zero", "due_date": day(-1), "recurrence": daily})
	// Due today: not moved yet.
	current := create(map[string]any{"title": "Standup", "due_date": day(0), "recurrence": daily})

	moved, err := taskrepeat.Sweep(context.Background(), d, deps.ReminderQueue)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 2 {
		t.Fatalf("moved %d, want 2: %+v", len(moved), moved)
	}
	if tr := task(done); tr.Status != "todo" || *tr.DueDate != day(0) || tr.Steps[0].Done {
		t.Fatalf("done task after sweep: %+v", tr)
	}
	if tr := task(open); tr.Status != "todo" || *tr.DueDate != day(0) {
		t.Fatalf("open task after sweep: %+v", tr)
	}
	var misses int
	d.QueryRow(`SELECT COUNT(*) FROM task_due_history WHERE task_id=$1 AND outcome='missed' AND due_date=$2`, open, day(-1)).Scan(&misses)
	if misses != 1 {
		t.Fatalf("misses = %d", misses)
	}
	if tr := task(current); *tr.DueDate != day(0) {
		t.Fatalf("today's task moved: %+v", tr)
	}
	// Running again changes nothing.
	if again, _ := taskrepeat.Sweep(context.Background(), d, deps.ReminderQueue); len(again) != 0 {
		t.Fatalf("second sweep moved %d", len(again))
	}

	// Skip moves past today without a miss.
	code, b := call("POST", "/api/tasks/"+strconv.FormatInt(current, 10)+"/skip", nil)
	if code != 200 {
		t.Fatalf("skip: %d %s", code, b)
	}
	if tr := task(current); *tr.DueDate != day(1) {
		t.Fatalf("skipped task: %+v", tr)
	}

	// Moving a daily task to a week clears the rule that no longer fits.
	monday := today.AddDate(0, 0, -((int(today.Weekday())+6)%7)).Format("2006-01-02")
	call("PUT", "/api/tasks/"+strconv.FormatInt(open, 10), map[string]any{"week_of": monday, "clear_due": true})
	if tr := task(open); tr.Recurrence.Recurring() {
		t.Fatalf("week task kept a daily rule: %+v", tr.Recurrence)
	}

	// Plans: every day at 09:00.
	code, b = call("POST", "/api/plans", map[string]any{"name": "Office start", "weekdays": []int{0, 1, 2, 3, 4, 5, 6}, "start_time": "09:00"})
	if code != 201 {
		t.Fatalf("create plan: %d %s", code, b)
	}
	var p struct{ ID int64 }
	json.Unmarshal(b, &p)
	pid := strconv.FormatInt(p.ID, 10)
	planned := create(map[string]any{"title": "Prepare the meeting", "due_date": day(2), "plan_id": p.ID})
	leftover := create(map[string]any{"title": "Send invoice", "due_date": day(-2), "plan_id": p.ID})
	loose := create(map[string]any{"title": "Call the bank"})

	code, b = call("GET", "/api/plans/day?date="+day(2), nil)
	var days []struct {
		Tasks    []struct{ ID int64 }
		LeftOver []struct{ ID int64 } `json:"left_over"`
	}
	json.Unmarshal(b, &days)
	if code != 200 || len(days) != 1 || len(days[0].Tasks) != 1 || days[0].Tasks[0].ID != planned || len(days[0].LeftOver) != 1 || days[0].LeftOver[0].ID != leftover {
		t.Fatalf("plan day: %d %s", code, b)
	}

	code, b = call("GET", "/api/plans/"+pid+"/candidates?date="+day(0), nil)
	var groups []struct {
		Key   string
		Tasks []struct{ ID int64 }
	}
	json.Unmarshal(b, &groups)
	found := map[string]bool{}
	for _, g := range groups {
		for _, x := range g.Tasks {
			found[g.Key+":"+strconv.FormatInt(x.ID, 10)] = true
		}
	}
	if code != 200 || !found["left_over:"+strconv.FormatInt(leftover, 10)] || !found["undated:"+strconv.FormatInt(loose, 10)] {
		t.Fatalf("candidates: %d %s", code, b)
	}

	// Pull the left-over into today: rescheduled, not a miss.
	code, b = call("POST", "/api/plans/"+pid+"/tasks", map[string]any{"task_id": leftover, "date": day(0)})
	if code != 200 {
		t.Fatalf("add to plan: %d %s", code, b)
	}
	if tr := task(leftover); *tr.DueDate != day(0) || tr.PlanID == nil || *tr.PlanID != p.ID {
		t.Fatalf("pulled task: %+v", tr)
	}
	var outcome string
	d.QueryRow(`SELECT outcome FROM task_due_history WHERE task_id=$1 AND due_date=$2`, leftover, day(-2)).Scan(&outcome)
	if outcome != "rescheduled" {
		t.Fatalf("left-over outcome = %q", outcome)
	}
	call("POST", "/api/plans/"+pid+"/tasks", map[string]any{"task_id": loose, "date": day(0)})
	if tr := task(loose); *tr.DueDate != day(0) || tr.PlanName == nil || *tr.PlanName != "Office start" {
		t.Fatalf("pulled undated task: %+v", tr)
	}

	// Deleting the plan keeps its tasks.
	call("DELETE", "/api/plans/"+pid, nil)
	if tr := task(planned); tr.ID == 0 || tr.PlanID != nil {
		t.Fatalf("task after plan delete: %+v", tr)
	}
}
