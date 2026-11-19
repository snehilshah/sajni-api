package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/dbtest"
)

// Needs a scratch Postgres. The parse picks the category in the same call;
// the capture files it, leaves "Others" blank, and never borrows a category
// of the other kind.
func TestCaptureFilesTheParsesCategory(t *testing.T) {
	d, err := db.New(dbtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000ca"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'capcat@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	d.Exec(`INSERT INTO fin_accounts (user_id, name, type, match_hints) VALUES ($1,'Kotak','savings','2851')`, uid)
	var food int64
	d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1,'Food','expense') RETURNING id`, uid).Scan(&food)
	d.Exec(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1,'Others','expense'), ($1,'Salary','income')`, uid)

	cats := txnCategories(context.Background(), d, uid)
	if cats == nil || len(cats.Expense) != 2 || len(cats.Income) != 1 {
		t.Fatalf("categories: %+v", cats)
	}

	canned := map[string]ai.ParsedTxn{
		"swiggy": {Amount: 240, Type: "expense", Description: "Swiggy", Date: "2026-11-16", Time: "13:05", AccountHint: "2851", RefID: "1", Category: "food"},
		"odd":    {Amount: 99, Type: "expense", Description: "Kiosk", Date: "2026-11-16", Time: "14:05", AccountHint: "2851", RefID: "2", Category: "Others"},
		"wrong":  {Amount: 50, Type: "expense", Description: "Shop", Date: "2026-11-16", Time: "15:05", AccountHint: "2851", RefID: "3", Category: "Salary"},
	}
	captureParserSwapped = true
	old := captureParser
	captureParser = func(_ context.Context, _ Deps, _, text, _ string) (ai.ParsedTxn, int, error) {
		return canned[text], 1, nil
	}
	defer func() { captureParser, captureParserSwapped = old, false }()
	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d, AILimiter: newAILimiter()})
	category := func(text string) *int64 {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(map[string]any{"text": text, "source": "sms", "sender": "JD-KOTAKB-S", "channel": "sms"})
		req := httptest.NewRequest("POST", "/api/finance/captures", &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 300 {
			t.Fatalf("capture %s: %d %s", text, rec.Code, rec.Body.String())
		}
		var id *int64
		d.QueryRow(`SELECT category_id FROM fin_transactions WHERE user_id=$1 AND description=$2`, uid, canned[text].Description).Scan(&id)
		return id
	}
	if got := category("swiggy"); got == nil || *got != food {
		t.Fatalf("Swiggy filed under %v, want Food %d", got, food)
	}
	// Not placed: the row lands in Unknown for the owner to sort.
	var unknown int64
	d.QueryRow(`SELECT fin_category_fallback($1, 'expense')`, uid).Scan(&unknown)
	if got := category("odd"); got == nil || *got != unknown {
		t.Fatalf("an automatic Others was filed: %v, want Unknown %d", got, unknown)
	}
	if got := category("wrong"); got == nil || *got != unknown {
		t.Fatalf("an expense was filed under an income category: %v", got)
	}
}
