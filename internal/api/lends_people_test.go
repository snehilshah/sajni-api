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

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run Lend
func TestLendPeopleEndpoints(t *testing.T) {
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
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'people@test', 'T')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var card, bank int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, statement_day, due_day) VALUES ($1,'Card','credit_card',1,20) RETURNING id`, uid).Scan(&card)
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Bank','savings') RETURNING id`, uid).Scan(&bank)
	var ids []int64
	for i := range 35 {
		var id int64
		d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at)
			VALUES ($1,$2,'expense',$3,'Electricity',NOW() - make_interval(days => $4)) RETURNING id`, uid, card, 100+i, i).Scan(&id)
		ids = append(ids, id)
	}
	var credit int64
	d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at) VALUES ($1,$2,'income',250,'UPI from Dad',NOW()) RETURNING id`, uid, bank).Scan(&credit)

	mux := http.NewServeMux()
	registerLendRoutes(mux, Deps{DB: d})
	call := func(method, path string, body any, out any) int {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if out != nil {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
			}
		}
		return rec.Code
	}

	var page struct {
		Items []lendCandidateResp `json:"items"`
		Next  *string             `json:"next"`
	}
	if code := call("GET", "/api/finance/lends/candidates?kind=paid_for&q=electric", nil, &page); code != 200 || len(page.Items) != 30 || page.Next == nil {
		t.Fatalf("first page: %d items=%d next=%v", code, len(page.Items), page.Next)
	}
	seen := map[int64]bool{}
	for _, it := range page.Items {
		seen[it.ID] = true
	}
	call("GET", "/api/finance/lends/candidates?kind=paid_for&cursor="+*page.Next, nil, &page)
	if len(page.Items) != 5 || page.Next != nil {
		t.Fatalf("second page: items=%d next=%v", len(page.Items), page.Next)
	}
	for _, it := range page.Items {
		if seen[it.ID] {
			t.Fatalf("cursor repeated transaction %d", it.ID)
		}
	}

	if code := call("POST", "/api/finance/lends/paid-for", map[string]any{"borrower": "Dad", "transaction_ids": ids[:2]}, nil); code != 201 {
		t.Fatalf("paid-for: %d", code)
	}
	if code := call("POST", "/api/finance/lends/paid-for", map[string]any{"borrower": "Dad", "transaction_ids": ids[:1]}, nil); code != 400 {
		t.Fatalf("re-marking a lend should be 400, got %d", code)
	}
	if code := call("POST", "/api/finance/lends/settle", map[string]any{"borrower": "Dad", "transaction_ids": []int64{credit}}, nil); code != 201 {
		t.Fatalf("settle: %d", code)
	}
	if code := call("POST", "/api/finance/lends/settle", map[string]any{"borrower": "Dad", "account_id": bank, "amount": 10}, nil); code != 201 {
		t.Fatalf("record settlement: %d", code)
	}

	var people []lendPersonResp
	call("GET", "/api/finance/lends/people", nil, &people)
	// Lends 100 + 101 = 201; credits 250 + 10 = 260 → 59 held for Dad.
	if len(people) != 1 || people[0].Outstanding != 0 || people[0].Credit != 59 || len(people[0].Settlements) != 2 {
		t.Fatalf("people: %+v", people)
	}
	// One settlement credit spread over two lends must still list once.
	tmux := http.NewServeMux()
	tmux.HandleFunc("GET /api/finance/transactions", listTransactions(Deps{DB: d}))
	treq := httptest.NewRequest("GET", "/api/finance/transactions", nil)
	treq = treq.WithContext(context.WithValue(treq.Context(), auth.ContextKey{}, uid))
	trec := httptest.NewRecorder()
	tmux.ServeHTTP(trec, treq)
	var txns []txnResp
	json.Unmarshal(trec.Body.Bytes(), &txns)
	if len(txns) != 37 {
		t.Fatalf("transactions listed %d, want 37 (%d %s)", len(txns), trec.Code, trec.Body.String()[:min(200, trec.Body.Len())])
	}
	for _, x := range txns {
		if x.ID == credit && (x.LendBorrower == nil || *x.LendBorrower != "Dad") {
			t.Fatalf("settlement row should carry the person: %+v", x)
		}
	}

	var lends []lendResp
	call("GET", "/api/finance/lends", nil, &lends)
	if len(lends) != 2 || lends[0].Origin != "paid_for" {
		t.Fatalf("lends: %+v", lends)
	}
	// Deleting a paid-for lend hands the expense back rather than deleting it.
	if code := call("DELETE", "/api/finance/lends/"+strconv.FormatInt(lends[0].ID, 10), nil, nil); code != 200 {
		t.Fatalf("delete paid-for: %d", code)
	}
	var typ string
	d.QueryRow(`SELECT type FROM fin_transactions WHERE id = $1`, lends[0].Repayments[0].TransactionID).Scan(&typ)
	if typ != "lend_repayment" {
		t.Fatalf("settlement credit should stay a settlement, got %s", typ)
	}
	var expenses int
	d.QueryRow(`SELECT COUNT(*) FROM fin_transactions WHERE user_id = $1 AND type = 'expense'`, uid).Scan(&expenses)
	if expenses != 34 {
		t.Fatalf("expenses after unmark = %d, want 34", expenses)
	}
	if code := call("DELETE", "/api/finance/lends/settlements/"+strconv.FormatInt(people[0].Settlements[0].ID, 10), nil, nil); code != 200 {
		t.Fatalf("unsettle: %d", code)
	}
}
