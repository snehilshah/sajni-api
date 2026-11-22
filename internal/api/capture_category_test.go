package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sajni/internal/ai"
	"sajni/internal/db"
	"sajni/internal/dbtest"
)

// Needs a scratch Postgres. A capture's category comes from the owner's
// rule, then the Decisions API (on the message and what was read), then the
// parse's own pick; an automatic capture leaves an unsure pick in Unknown.
func TestCaptureCategoryOrder(t *testing.T) {
	d, err := db.New(dbtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000c4"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'capcat@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var food, transport int64
	d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1, 'Food', 'expense') RETURNING id`, uid).Scan(&food)
	d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1, 'Transport', 'expense') RETURNING id`, uid).Scan(&transport)
	d.Exec(`INSERT INTO fin_merchant_categories (user_id, merchant, category_id) VALUES ($1, 'uber india', $2)`, uid, transport)

	var calls int
	var lastInput string
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if down {
			w.WriteHeader(503)
			return
		}
		var body struct {
			Input string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		lastInput = body.Input
		conf := "0.92"
		if strings.Contains(body.Input, "UNSURE") {
			conf = "0.31"
		}
		w.Write([]byte(`{"answers":{"q":{"type":"choice","choice":"Food","confidence":` + conf + `}},"usage":{"total_input_tokens":120}}`))
	}))
	defer srv.Close()
	deps := Deps{DB: d, AI: ai.NewDecisionsOnly("k", srv.URL)}
	now := time.Now()
	resolve := func(desc, msg string, auto bool) string {
		out := resolveParsedTransaction(context.Background(), deps, uid,
			ai.ParsedTxn{Amount: 249, Type: "expense", Description: desc, Category: "Transport"}, msg, time.UTC, now, auto)
		return out["category_name"].(string)
	}

	if got := resolve("Uber India", "Rs 249 to UBER INDIA", true); got != "Transport" || calls != 0 {
		t.Fatalf("rule: %q, decisions calls %d", got, calls)
	}
	if got := resolve("Swiggy", "Rs 249 debited to SWIGGY", true); got != "Food" {
		t.Fatalf("decisions over the parse's pick: %q", got)
	}
	if !strings.Contains(lastInput, "249") || !strings.Contains(lastInput, "Swiggy") || !strings.Contains(lastInput, "debited to SWIGGY") {
		t.Fatalf("decisions input: %q", lastInput)
	}
	if got := resolve("Shop", "UNSURE Rs 249", true); got != "" {
		t.Fatalf("auto, unsure: %q, want Unknown (blank)", got)
	}
	if got := resolve("Shop", "UNSURE Rs 249", false); got != "Food" {
		t.Fatalf("typed, unsure: %q, want the pick", got)
	}
	down = true
	if got := resolve("Swiggy", "Rs 249 debited to SWIGGY", true); got != "Transport" {
		t.Fatalf("decisions down: %q, want the parse's pick", got)
	}
}
