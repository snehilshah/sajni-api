package ai

import (
	"context"
	"os"
	"testing"

	"sajni/internal/db"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/ai
func TestFinanceSetupTools(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	uid := "01900000-0000-7000-8000-0000000000f5"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	_, err = d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'setup@test', 'T')`, uid)
	must(err)
	_, err = d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	must(err)
	balance := func(id int64) (v float64) {
		d.QueryRow(`SELECT opening_balance + COALESCE((SELECT SUM(CASE WHEN type IN ('income','refund','transfer_in','lend_repayment') THEN amount
			WHEN type IN ('expense','transfer_out','lend') THEN -amount ELSE 0 END) FROM fin_transactions WHERE account_id = $1), 0)
			FROM fin_accounts WHERE id = $1`, id).Scan(&v)
		return
	}
	newAcct := func(args map[string]any) int64 {
		t.Helper()
		data, _, err := saveAccountTool(ctx, d, uid, 0, args)
		must(err)
		return data.(map[string]any)["id"].(int64)
	}

	bank := newAcct(map[string]any{"name": "Bank", "type": "savings", "opening_balance": 50000.0})
	card := newAcct(map[string]any{"name": "Card", "type": "credit_card", "statement_day": 1.0, "due_day": 16.0})
	if _, _, err := saveAccountTool(ctx, d, uid, 0, map[string]any{"name": "X", "type": "loan"}); err == nil {
		t.Fatal("an unknown account type was accepted")
	}
	_, _, err = saveAccountTool(ctx, d, uid, card, map[string]any{"opening_balance": -10000.0})
	must(err)
	if b := balance(card); b != -10000 {
		t.Fatalf("card opening = %v, want -10000", b)
	}

	// A card bill paid from the bank: two paired legs, no spending.
	data, _, err := createTxnTool(ctx, d, uid, map[string]any{"account_id": float64(bank), "type": "transfer",
		"to_account_id": float64(card), "amount": 10000.0, "description": "Card bill", "note": "ref 42 #bills"})
	must(err)
	out := data.(map[string]any)["id"].(int64)
	if balance(bank) != 40000 || balance(card) != 0 {
		t.Fatalf("after the bill: bank %v card %v, want 40000 and 0", balance(bank), balance(card))
	}
	var tags int
	d.QueryRow(`SELECT COUNT(*) FROM tags WHERE user_id = $1 AND entity_type = 'transaction'`, uid).Scan(&tags)
	if tags != 2 {
		t.Fatalf("got %d transaction tags, want one per leg", tags)
	}

	// A note can be set and cleared on an expense.
	data, _, err = createTxnTool(ctx, d, uid, map[string]any{"account_id": float64(card), "amount": 22.0, "description": "Tea"})
	must(err)
	tea := data.(map[string]any)["id"].(int64)
	_, _, err = updateTxnTool(ctx, d, uid, map[string]any{"id": float64(tea), "note": "with Jai"})
	must(err)
	var note string
	d.QueryRow(`SELECT note FROM fin_transactions WHERE id = $1`, tea).Scan(&note)
	if note != "with Jai" {
		t.Fatalf("note = %q", note)
	}

	// Deleting one leg of a transfer deletes both and their tags.
	_, _, err = deleteTxnTool(ctx, d, uid, map[string]any{"id": float64(out)})
	must(err)
	if balance(bank) != 50000 || balance(card) != -10022 {
		t.Fatalf("after delete: bank %v card %v", balance(bank), balance(card))
	}
	d.QueryRow(`SELECT COUNT(*) FROM tags WHERE user_id = $1 AND entity_type = 'transaction'`, uid).Scan(&tags)
	if tags != 0 {
		t.Fatalf("%d tags left behind", tags)
	}
	if _, _, err := deleteTxnTool(ctx, d, uid, map[string]any{"id": float64(out)}); err == nil {
		t.Fatal("deleting a missing transaction succeeded")
	}

	// An investment, a payment into it, an update; deleting the payment takes it back out.
	data, _, err = createInvestmentTool(ctx, d, uid, map[string]any{"name": "IPO", "account_id": float64(bank), "start_date": "2026-10-27"})
	must(err)
	inv := data.(map[string]any)["id"].(int64)
	var freq string
	d.QueryRow(`SELECT frequency FROM fin_investments WHERE id = $1`, inv).Scan(&freq)
	if freq != "lumpsum" {
		t.Fatalf("frequency = %q, want lumpsum for type other", freq)
	}
	data, _, err = createTxnTool(ctx, d, uid, map[string]any{"account_id": float64(bank), "type": "transfer",
		"to_investment_id": float64(inv), "amount": 13428.0, "description": "IPO"})
	must(err)
	pay := data.(map[string]any)["id"].(int64)
	_, _, err = updateInvestmentTool(ctx, d, uid, map[string]any{"investment_id": float64(inv), "current_value": 15000.0})
	must(err)
	var invested, current float64
	d.QueryRow(`SELECT invested_amount, current_value FROM fin_investments WHERE id = $1`, inv).Scan(&invested, &current)
	if invested != 13428 || current != 15000 {
		t.Fatalf("invested %v current %v", invested, current)
	}
	_, _, err = deleteTxnTool(ctx, d, uid, map[string]any{"id": float64(pay)})
	must(err)
	d.QueryRow(`SELECT invested_amount FROM fin_investments WHERE id = $1`, inv).Scan(&invested)
	if invested != 0 || balance(bank) != 50000 {
		t.Fatalf("after deleting the payment: invested %v bank %v", invested, balance(bank))
	}
}
