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

	"sajni/internal/auth"
	"sajni/internal/db"
)

// A repayment's title and note can be edited; its money can't.
// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run Repayment
func TestRepaymentEditTitleOnly(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-000000000003"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'repay@test', 'T')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var card, bank, bill, credit int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, statement_day, due_day) VALUES ($1,'Card','credit_card',1,20) RETURNING id`, uid).Scan(&card)
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Bank','savings') RETURNING id`, uid).Scan(&bank)
	d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at) VALUES ($1,$2,'expense',1499,'Jacket',NOW()) RETURNING id`, uid, card).Scan(&bill)
	d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at) VALUES ($1,$2,'income',200,'Manica Cashback',NOW()) RETURNING id`, uid, bank).Scan(&credit)

	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d})
	call := func(method, path string, body any) int {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("POST", "/api/finance/lends/paid-for", map[string]any{"borrower": "Monica", "transaction_ids": []int64{bill}}); code != 201 {
		t.Fatalf("paid for: %d", code)
	}
	if code := call("POST", "/api/finance/lends/settle", map[string]any{"borrower": "Monica", "transaction_ids": []int64{credit}}); code != 201 {
		t.Fatalf("settle: %d", code)
	}
	path := "/api/finance/transactions/" + strconv.FormatInt(credit, 10)

	if code := call("PUT", path, map[string]any{"description": "Monica Cashback", "note": "Amazon offer"}); code != 200 {
		t.Fatalf("rename repayment: %d", code)
	}
	var desc, note, typ string
	var amount float64
	d.QueryRow(`SELECT description, note, type, amount FROM fin_transactions WHERE id=$1`, credit).Scan(&desc, &note, &typ, &amount)
	if desc != "Monica Cashback" || note != "Amazon offer" || typ != "lend_repayment" || amount != 200 {
		t.Fatalf("after rename: %q %q %s %v", desc, note, typ, amount)
	}

	if code := call("PUT", path, map[string]any{"description": "x", "amount": 150}); code != http.StatusConflict {
		t.Fatalf("amount edit on a repayment: %d, want 409", code)
	}
	d.QueryRow(`SELECT description, amount FROM fin_transactions WHERE id=$1`, credit).Scan(&desc, &amount)
	if desc != "Monica Cashback" || amount != 200 {
		t.Fatalf("refused edit still wrote: %q %v", desc, amount)
	}

	// A lend itself stays locked.
	if code := call("PUT", "/api/finance/transactions/"+strconv.FormatInt(bill, 10), map[string]any{"description": "x"}); code != http.StatusConflict {
		t.Fatalf("lend edit: %d, want 409", code)
	}
}
