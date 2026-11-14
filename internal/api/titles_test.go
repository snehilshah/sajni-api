package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"sajni/internal/auth"
	"sajni/internal/db"
)

// "Sew" → Sewri (₹22 Smoke on RuPay, used most), not the Office Smoke rows.
func TestSuggestTitles(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000e1"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'titles@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	var rupay, smoke int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'RuPay','credit_card') RETURNING id`, uid).Scan(&rupay)
	d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind, color) VALUES ($1,'Smoke','expense','#000') RETURNING id`, uid).Scan(&smoke)
	for _, row := range []struct {
		desc string
		amt  float64
	}{{"Sewri", 22}, {"Sewri", 22}, {"Office Smoke", 22}, {"New Sewing kit", 300}} {
		if _, err := d.Exec(`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at, slate_id)
			VALUES ($1,$2,$3,'expense',$4,$5,NOW(),(SELECT id FROM fin_slates WHERE user_id=$1))`,
			uid, rupay, smoke, row.amt, row.desc); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d, AILimiter: newAILimiter()})
	req := httptest.NewRequest("GET", "/api/finance/titles?q=sew&type=expense", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var got []titleSuggestion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(got) != 2 || got[0].Title != "Sewri" || got[0].Uses != 2 || got[0].AccountID == nil || *got[0].AccountID != rupay || got[0].CategoryID == nil || *got[0].CategoryID != smoke {
		t.Fatalf("got %+v", got)
	}
	if got[1].Title != "New Sewing kit" {
		t.Fatalf("word match: %+v", got[1])
	}
}
