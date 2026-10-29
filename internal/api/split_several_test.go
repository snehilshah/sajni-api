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

// Creating a bill split three ways, splitting an existing one, and reading
// both back from the transaction list.
// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run Split
func TestSplitSeveralEndpoints(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-00000000000c"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'split-api@test', 'T')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var card int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, statement_day, due_day) VALUES ($1,'Card','credit_card',1,20) RETURNING id`, uid).Scan(&card)

	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d})
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

	var created struct {
		ID int64 `json:"id"`
	}
	if code := call("POST", "/api/finance/transactions", map[string]any{
		"account_id": card, "type": "expense", "amount": 900, "description": "Dinner",
		"splits": []map[string]any{{"borrower": "Rahul", "share": 300}, {"borrower": "Priya", "share": 300}},
	}, &created); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create split: %d", code)
	}

	var existing int64
	d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, slate_id)
		VALUES ($1,$2,'expense',600,'Tickets',NOW() - INTERVAL '1 hour',(SELECT id FROM fin_slates WHERE user_id=$1)) RETURNING id`, uid, card).Scan(&existing)
	var marked struct {
		LendIDs []int64 `json:"lend_ids"`
	}
	if code := call("POST", "/api/finance/lends/paid-for", map[string]any{
		"transaction_ids": []int64{existing},
		"shares":          []map[string]any{{"borrower": "Rahul", "share": 300}, {"borrower": "Aman", "share": 300}},
	}, &marked); code != http.StatusCreated || len(marked.LendIDs) != 2 {
		t.Fatalf("split existing: %d %v", code, marked.LendIDs)
	}

	var list []txnResp
	call("GET", "/api/finance/transactions", nil, &list)
	if len(list) != 2 {
		t.Fatalf("share rows must fold into their bills, got %d rows", len(list))
	}
	byID := map[int64]txnResp{}
	for _, row := range list {
		byID[row.ID] = row
	}
	dinner := byID[created.ID]
	if dinner.Amount != 300 || len(dinner.Splits) != 2 || dinner.Split == nil || dinner.Split.Borrower != "Rahul" {
		t.Fatalf("dinner: amount %.2f splits %+v split %+v", dinner.Amount, dinner.Splits, dinner.Split)
	}
	if dinner.Splits[1].Borrower != "Priya" || dinner.Splits[1].Share != 300 {
		t.Fatalf("dinner second share: %+v", dinner.Splits[1])
	}
	tickets := byID[existing]
	if tickets.Amount != 0 || len(tickets.Splits) != 2 {
		t.Fatalf("tickets: amount %.2f splits %+v", tickets.Amount, tickets.Splits)
	}

	// A bill the others share entirely still takes edits (its ₹0 part included).
	if code := call("PUT", "/api/finance/transactions/"+strconv.FormatInt(existing, 10), map[string]any{
		"amount": 0, "description": "Movie tickets",
	}, nil); code != http.StatusOK {
		t.Fatalf("edit ₹0 part: %d", code)
	}
	// Deleting a shared bill names everyone on it.
	var refused struct {
		Error string `json:"error"`
	}
	if code := call("DELETE", "/api/finance/transactions/"+strconv.FormatInt(created.ID, 10), nil, &refused); code != http.StatusConflict ||
		refused.Error != "This bill is split with Rahul, Priya. Remove the split first." {
		t.Fatalf("delete: %d %q", code, refused.Error)
	}
	// An unsplit bill still lists with no shares.
	var plain struct {
		ID int64 `json:"id"`
	}
	call("POST", "/api/finance/transactions", map[string]any{"account_id": card, "type": "expense", "amount": 50, "description": "Tea"}, &plain)
	call("GET", "/api/finance/transactions", nil, &list)
	for _, row := range list {
		if row.ID == plain.ID && (row.Split != nil || row.Splits == nil || len(row.Splits) != 0) {
			t.Fatalf("plain bill: split %+v splits %+v", row.Split, row.Splits)
		}
	}
}
