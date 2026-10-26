package api

import (
	"encoding/json"
	"os"
	"testing"

	"sajni/internal/db"
)

func TestPlanProgress(t *testing.T) {
	run := &progressRun{id: 1, fromEps: 3, fromSeas: 1}
	tests := []struct {
		name                     string
		run                      *progressRun
		prevEps, prevSeas        int
		eps, seas                int
		want                     progressAction
		wantFromEps, wantFromSea int
	}{
		{"first step starts a run", nil, 3, 1, 4, 1, progressInsert, 3, 1},
		{"backlog jump starts a run", nil, 0, 0, 27, 3, progressInsert, 0, 0},
		{"next step extends the run", run, 4, 1, 5, 1, progressUpdate, 3, 1},
		{"stepping back inside the run updates it", run, 5, 1, 4, 1, progressUpdate, 3, 1},
		{"stepping back to the start drops it", run, 4, 1, 3, 1, progressDelete, 0, 0},
		{"stepping back past the start drops it", run, 4, 1, 2, 1, progressDelete, 0, 0},
		{"season-only advance extends the run", run, 3, 1, 3, 2, progressUpdate, 3, 1},
		{"backward with no run logs nothing", nil, 5, 1, 4, 1, progressNone, 0, 0},
	}
	for _, tt := range tests {
		got, fe, fs := planProgress(tt.run, tt.prevEps, tt.prevSeas, tt.eps, tt.seas)
		if got != tt.want || fe != tt.wantFromEps || fs != tt.wantFromSea {
			t.Errorf("%s: got (%v, %d, %d), want (%v, %d, %d)", tt.name, got, fe, fs, tt.want, tt.wantFromEps, tt.wantFromSea)
		}
	}
}

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api
func TestLogMediaProgressMerges(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	uid := "01900000-0000-7000-8000-000000000002"
	_, _ = d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'media@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := d.QueryRow(`INSERT INTO media (user_id, title, type) VALUES ($1, 'Show', 'show') RETURNING id`, uid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	seasons := []int{10, 10}
	type row struct {
		Kind string
		Meta map[string]any
	}
	events := func() []row {
		t.Helper()
		rows, err := d.Query(`SELECT kind, meta::text FROM media_events WHERE media_id = $1 ORDER BY id`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			var raw string
			if err := rows.Scan(&r.Kind, &raw); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal([]byte(raw), &r.Meta)
			out = append(out, r)
		}
		return out
	}

	// Four +1 taps: one entry, S1E3 → S1E7.
	for eps := 3; eps < 7; eps++ {
		logMediaProgress(d, uid, id, eps, 1, eps+1, 1, 20, seasons)
	}
	got := events()
	if len(got) != 1 || got[0].Meta["from_episode"] != 3.0 || got[0].Meta["episode"] != 7.0 {
		t.Fatalf("after taps: %+v", got)
	}
	// Back to the start: the entry goes.
	for eps := 7; eps > 3; eps-- {
		logMediaProgress(d, uid, id, eps, 1, eps-1, 1, 20, seasons)
	}
	if got := events(); len(got) != 0 {
		t.Fatalf("after stepping back: %+v", got)
	}
	// A rating in between ends the run; the next step starts a new one.
	logMediaProgress(d, uid, id, 3, 1, 12, 2, 20, seasons)
	logMediaEvent(d, uid, id, "rating", map[string]any{"rating": 4})
	logMediaProgress(d, uid, id, 12, 2, 13, 2, 20, seasons)
	got = events()
	if len(got) != 3 || got[0].Meta["season"] != 2.0 || got[0].Meta["episode"] != 2.0 || got[2].Meta["from_episodes_watched"] != 12.0 {
		t.Fatalf("after rating: %+v", got)
	}
	// Outside the window: a new entry.
	_, _ = d.Exec(`UPDATE media_events SET created_at = NOW() - INTERVAL '31 minutes' WHERE media_id = $1`, id)
	logMediaProgress(d, uid, id, 13, 2, 14, 2, 20, seasons)
	if got := events(); len(got) != 4 {
		t.Fatalf("after window: %+v", got)
	}
	_, _ = d.Exec(`DELETE FROM users WHERE id = $1`, uid)
}
