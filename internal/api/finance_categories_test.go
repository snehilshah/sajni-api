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

func TestDefaultCategoryNamesAreKindSpecificAndCaseInsensitive(t *testing.T) {
	if !isDefaultCategoryName("expense", " food & dining ") {
		t.Error("expense default not recognized case-insensitively")
	}
	if !isDefaultCategoryName("income", "SALARY") {
		t.Error("income default not recognized case-insensitively")
	}
	if !isDefaultCategoryName("income", "other") || !isDefaultCategoryName("expense", "OTHERS") {
		t.Error("Other/Others alias should match both default kinds")
	}
	if isDefaultCategoryName("income", "Food & Dining") {
		t.Error("expense default must remain valid as a distinct income category")
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
