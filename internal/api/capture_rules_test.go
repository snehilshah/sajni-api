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

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run Capture
func TestCaptureRules(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
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
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'capture@test', 'T')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	cat := func(name, kind string) (id int64) {
		d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1,$2,$3) RETURNING id`, uid, name, kind).Scan(&id)
		return
	}
	smoke, others, groceries := cat("Smoke", "expense"), cat("Others", "expense"), cat("Groceries", "expense")
	cat("Cashback", "income")
	var bank, card int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Kotak','savings') RETURNING id`, uid).Scan(&bank)
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Card','credit_card') RETURNING id`, uid).Scan(&card)

	rule := func(merchant string) string {
		var name string
		d.QueryRow(`SELECT c.name FROM fin_merchant_categories m JOIN fin_categories c ON c.id = m.category_id WHERE m.user_id = $1 AND m.merchant = $2`, uid, merchant).Scan(&name)
		return name
	}

	// Lookup: prefix for names the bank cut, kind-aware, never "Others".
	learnMerchantCategory(d, uid, "Salman Mohammad Ismail Shaikh", &smoke)
	learnMerchantCategory(d, uid, "Amazon Pay", &others)
	if rule("amazon pay") != "" {
		t.Fatal(`"Others" was learned`)
	}
	if _, name := lookupMerchantCategory(d, uid, "Salman Mohammad Isma", "expense"); name != "Smoke" {
		t.Fatalf("cut name: got %q", name)
	}
	if id, _ := lookupMerchantCategory(d, uid, "Salman Mohammad Isma", "income"); id != nil {
		t.Fatal("an expense rule filled an income")
	}
	if id, _ := lookupMerchantCategory(d, uid, "Salman", "expense"); id != nil {
		t.Fatal("a short name matched by prefix")
	}

	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d})
	call := func(method, path string, body any, direct bool) (int, map[string]any) {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		if direct {
			req.Header.Set("X-Sajni-Direct", "1")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// A capture filed by the phone teaches nothing; the owner's own entry does.
	code, out := call("POST", "/api/finance/transactions", map[string]any{"account_id": card, "type": "expense", "amount": 1522, "description": "Amazon Pay In G", "category_id": groceries, "txn_at": "2026-11-12T22:26:00+05:30"}, true)
	if code != 201 || rule("amazon pay in g") != "" {
		t.Fatalf("auto-capture learned a rule: %d %v", code, out)
	}
	captured := int64(out["id"].(float64))
	if code, _ := call("POST", "/api/finance/transactions", map[string]any{"account_id": bank, "type": "expense", "amount": 176, "description": "Apple Chemist", "category_id": groceries, "txn_at": "2026-11-09T20:58:00+05:30"}, false); code != 201 || rule("apple chemist") != "Groceries" {
		t.Fatalf("manual entry didn't learn: %d", code)
	}

	// Renaming and re-filing a captured row teaches both names.
	if code, _ := call("PUT", "/api/finance/transactions/"+strconv.FormatInt(captured, 10), map[string]any{"description": "Amazon Now groceries", "category_id": groceries}, false); code != 200 {
		t.Fatalf("edit: %d", code)
	}
	if rule("amazon pay in g") != "Groceries" || rule("amazon now groceries") != "Groceries" {
		t.Fatalf("edit didn't teach: %q %q", rule("amazon pay in g"), rule("amazon now groceries"))
	}

	// The bank confirming an RD instalment Sajni already posted is not a new expense.
	var rd int64
	d.QueryRow(`INSERT INTO fin_investments (user_id, name, type) VALUES ($1,'Savings RD','rd') RETURNING id`, uid).Scan(&rd)
	d.Exec(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, investment_id, slate_id)
		VALUES ($1,$2,'transfer_out',10000,'Savings RD (auto)','2026-11-12T00:00:00+05:30',$3,(SELECT id FROM fin_slates WHERE user_id=$1))`, uid, bank, rd)
	_, out = call("POST", "/api/finance/transactions", map[string]any{"account_id": bank, "type": "expense", "amount": 10000, "description": "Standing Instruction", "txn_at": "2026-11-12T07:17:00+05:30"}, true)
	if out["duplicate"] != true || out["expected"] != "Savings RD" {
		t.Fatalf("RD confirmation filed as an expense: %v", out)
	}
	if _, out = call("POST", "/api/finance/transactions", map[string]any{"account_id": bank, "type": "expense", "amount": 10000, "description": "Rent", "txn_at": "2026-11-12T07:17:00+05:30"}, false); out["duplicate"] == true {
		t.Fatal("a manual ₹10,000 expense was taken for the RD")
	}
}

// Needs a scratch Postgres. An RD maturing: own money back is a transfer,
// the rest Interest; the investment closes; deleting the transfer undoes both.
func TestInvestmentMaturity(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
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
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'maturity@test', 'T')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1,'Interest','income')`, uid)
	var bank, rd int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Kotak','savings') RETURNING id`, uid).Scan(&bank)
	d.QueryRow(`INSERT INTO fin_investments (user_id, name, type, account_id, invested_amount, current_value, maturity_date)
		VALUES ($1,'Savings RD','rd',$2,150000,150000,'2026-12-12') RETURNING id`, uid, bank).Scan(&rd)

	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d})
	call := func(method, path string, body any, direct bool) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		if direct {
			req.Header.Set("X-Sajni-Direct", "1")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return out
	}
	state := func() (invested, balance, income float64) {
		d.QueryRow(`SELECT invested_amount FROM fin_investments WHERE id = $1`, rd).Scan(&invested)
		d.QueryRow(`SELECT COALESCE(SUM(`+db.BalanceDelta("t")+`),0) FROM fin_transactions t WHERE t.account_id = $1`, bank).Scan(&balance)
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND type = 'income'`, uid).Scan(&income)
		return
	}

	// The bank's SMS on the day, captured by the phone as income.
	out := call("POST", "/api/finance/transactions", map[string]any{"account_id": bank, "type": "income", "amount": 157420, "description": "Kotak RD", "txn_at": "2026-12-12T10:05:00+05:30"}, true)
	if out["matured"] != "Savings RD" || out["interest"] != 7420.0 {
		t.Fatalf("maturity not recognised: %v", out)
	}
	if inv, bal, inc := state(); inv != 0 || bal != 157420 || inc != 7420 {
		t.Fatalf("after maturity: invested %v balance %v income %v", inv, bal, inc)
	}
	var auto bool
	var value float64
	d.QueryRow(`SELECT auto_debit, current_value FROM fin_investments WHERE id = $1`, rd).Scan(&auto, &value)
	if auto || value != 0 {
		t.Fatalf("not closed: auto %v value %v", auto, value)
	}

	// Deleting the transfer brings the money back into the RD and drops the interest.
	call("DELETE", "/api/finance/transactions/"+strconv.FormatInt(int64(out["id"].(float64)), 10), nil, false)
	if inv, bal, inc := state(); inv != 150000 || bal != 0 || inc != 0 {
		t.Fatalf("after undo: invested %v balance %v income %v", inv, bal, inc)
	}

	// Breaking it early for less than was put in: all own money, nothing earned.
	call("POST", "/api/finance/transactions", map[string]any{"account_id": bank, "type": "transfer", "from_investment_id": rd, "amount": 60000, "description": "Broke the RD", "txn_at": "2026-11-20T10:00:00+05:30"}, false)
	if inv, bal, inc := state(); inv != 90000 || bal != 60000 || inc != 0 {
		t.Fatalf("partial: invested %v balance %v income %v", inv, bal, inc)
	}
	// An ordinary credit far from maturity stays income.
	out = call("POST", "/api/finance/transactions", map[string]any{"account_id": bank, "type": "income", "amount": 90000, "description": "Bonus", "txn_at": "2026-11-25T10:00:00+05:30"}, true)
	if out["matured"] != nil {
		t.Fatalf("a bonus was taken for the RD: %v", out)
	}
}
