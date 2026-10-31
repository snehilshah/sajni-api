package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"sajni/internal/auth"
	"sajni/internal/db"
)

// ?logged= sets a check-in instead of flipping it, so the offline outbox can
// resend one safely; without it the endpoints still toggle.
// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run HabitSet
func TestHabitSet(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-00000000000e"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'habit-set@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var daily, weekly int64
	d.QueryRow(`INSERT INTO habits (user_id, name, frequency) VALUES ($1, 'Read', 'daily') RETURNING id`, uid).Scan(&daily)
	d.QueryRow(`INSERT INTO habits (user_id, name, frequency) VALUES ($1, 'Oil', 'weekly') RETURNING id`, uid).Scan(&weekly)

	mux := http.NewServeMux()
	registerHabitRoutes(mux, Deps{DB: d})
	post := func(path string) bool {
		t.Helper()
		req := httptest.NewRequest("POST", path, nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out struct {
			Logged bool `json:"logged"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		return out.Logged
	}
	day := "2026-10-01"
	logs := func(id int64) (n int) {
		d.QueryRow(`SELECT COUNT(*) FROM habit_logs WHERE habit_id = $1`, id).Scan(&n)
		return
	}
	base := "/api/habits/" + strconv.FormatInt(daily, 10) + "/log/" + day
	if !post(base+"?logged=true") || !post(base+"?logged=true") || logs(daily) != 1 {
		t.Fatalf("set done twice: %d logs", logs(daily))
	}
	if post(base+"?logged=false") || post(base+"?logged=false") || logs(daily) != 0 {
		t.Fatalf("set not done twice: %d logs", logs(daily))
	}
	if !post(base) || post(base) {
		t.Fatal("without logged= it still toggles")
	}
	period := "/api/habits/" + strconv.FormatInt(weekly, 10) + "/period/" + day
	if !post(period+"?logged=true") || !post(period+"?logged=true") || logs(weekly) != 1 {
		t.Fatalf("period set twice: %d logs", logs(weekly))
	}
	if post(period+"?logged=false") || logs(weekly) != 0 {
		t.Fatal("period unset")
	}
}
