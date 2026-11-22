package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/dbtest"
)

func TestCategoryNameKey(t *testing.T) {
	cases := map[string]string{
		" Groceries ": "groceries",
		"GROCERIES":   "groceries",
		"Other":       "others",
		" others ":    "others",
	}
	for input, want := range cases {
		if got := categoryNameKey(input); got != want {
			t.Errorf("categoryNameKey(%q)=%q, want %q", input, got, want)
		}
	}
}

// Unknown: listed for every user, one per kind, flagged; can't be deleted or
// renamed, only recoloured.
func TestUnknownCategoryIsProtected(t *testing.T) {
	dsn := dbtest.URL(t)
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000c7"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'unknown@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d, AILimiter: newAILimiter()})
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	var cats []struct {
		ID       int64
		Name     string
		Kind     string
		Fallback bool
	}
	json.Unmarshal(call("GET", "/api/finance/categories", nil).Body.Bytes(), &cats)
	var unknown int64
	kinds := map[string]bool{}
	for _, c := range cats {
		if c.Fallback {
			if c.Name != "Unknown" || kinds[c.Kind] {
				t.Fatalf("fallback: %+v", c)
			}
			kinds[c.Kind] = true
			if c.Kind == "expense" {
				unknown = c.ID
			}
		}
	}
	if !kinds["expense"] || !kinds["income"] {
		t.Fatalf("one Unknown per kind: %+v", cats)
	}
	path := fmt.Sprintf("/api/finance/categories/%d", unknown)
	if rec := call("DELETE", path, nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete Unknown: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("PUT", path, map[string]any{"name": "Misc"}); rec.Code != http.StatusConflict {
		t.Fatalf("rename Unknown: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("PUT", path, map[string]any{"name": "Unknown", "color": "#112233"}); rec.Code != 200 {
		t.Fatalf("recolour Unknown: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("POST", "/api/finance/categories", map[string]any{"name": "unknown", "kind": "expense"}); rec.Code != http.StatusConflict {
		t.Fatalf("a second Unknown: %d %s", rec.Code, rec.Body.String())
	}
}

// A starter category renamed or deleted stays that way: listing again must
// not bring the old name back (Food & Dining → Food made two categories),
// and the transactions follow the rename, since they point at its id.
func TestRenamedStarterCategoryStaysRenamed(t *testing.T) {
	dsn := dbtest.URL(t)
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000c8"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'rename-cat@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d, AILimiter: newAILimiter()})
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	list := func() map[string]int64 {
		t.Helper()
		var cats []struct {
			ID   int64
			Name string
			Kind string
		}
		json.Unmarshal(call("GET", "/api/finance/categories", nil).Body.Bytes(), &cats)
		out := map[string]int64{}
		for _, c := range cats {
			out[c.Kind+"/"+c.Name] = c.ID
		}
		return out
	}
	cats := list()
	food, transport := cats["expense/Food & Dining"], cats["expense/Transport"]
	if food == 0 || transport == 0 || cats["income/Salary"] == 0 {
		t.Fatalf("starters not seeded: %v", cats)
	}
	var acct, txn int64
	if err := d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Bank','savings') RETURNING id`, uid).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, slate_id, txn_at)
		VALUES ($1,$2,$3,'expense',120,'Lunch',(SELECT id FROM fin_slates WHERE user_id=$1 AND is_plain),NOW()) RETURNING id`, uid, acct, food).Scan(&txn); err != nil {
		t.Fatal(err)
	}

	if rec := call("PUT", fmt.Sprintf("/api/finance/categories/%d", food), map[string]any{"name": "Food"}); rec.Code != 200 {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("DELETE", fmt.Sprintf("/api/finance/categories/%d", transport), nil); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	cats = list()
	if cats["expense/Food"] != food {
		t.Fatalf("rename kept its id: %v", cats)
	}
	if cats["expense/Food & Dining"] != 0 || cats["expense/Transport"] != 0 {
		t.Fatalf("a renamed or deleted starter came back: %v", cats)
	}
	var name string
	d.QueryRow(`SELECT c.name FROM fin_transactions t JOIN fin_categories c ON c.id = t.category_id WHERE t.id = $1`, txn).Scan(&name)
	if name != "Food" {
		t.Fatalf("transaction's category = %q, want Food", name)
	}
	// The old starter name is free again.
	if rec := call("POST", "/api/finance/categories", map[string]any{"name": "Transport", "kind": "expense"}); rec.Code != 201 {
		t.Fatalf("re-create Transport: %d %s", rec.Code, rec.Body.String())
	}
}
