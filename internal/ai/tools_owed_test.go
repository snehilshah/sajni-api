package ai

import (
	"context"
	"os"
	"testing"

	"sajni/internal/db"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/ai
func TestOwedTools(t *testing.T) {
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
	uid := "01900000-0000-7000-8000-0000000000f6"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	_, err = d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'owedtools@test', 'T')`, uid)
	must(err)
	_, err = d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	must(err)
	var kotak, neo int64
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Kotak','savings') RETURNING id`, uid).Scan(&kotak))
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Neo','investment') RETURNING id`, uid).Scan(&neo))
	balance := func(id int64) (v float64) {
		d.QueryRow(`SELECT opening_balance + COALESCE((SELECT SUM(`+db.BalanceDelta("t")+`) FROM fin_transactions t WHERE t.account_id = $1),0)
			FROM fin_accounts WHERE id = $1`, id).Scan(&v)
		return
	}
	youOwe := func() float64 {
		t.Helper()
		data, _, err := listMoneyOwedTool(ctx, d, uid)
		must(err)
		return data.(map[string]any)["total_you_owe"].(float64)
	}

	// Papa's leftover share money: ₹1 into Kotak, ₹2,723.33 into Neo.
	_, _, err = borrowTool(ctx, d, uid, map[string]any{"person": "Papa", "account_id": float64(kotak), "amount": 1.0, "description": "Left from the share money", "date": "2026-11-02", "time": "14:17"})
	must(err)
	_, _, err = borrowTool(ctx, d, uid, map[string]any{"person": "papa", "account_id": float64(neo), "amount": 2723.33, "description": "Left from the share money", "date": "2026-11-02", "time": "14:19"})
	must(err)
	if balance(kotak) != 1 || balance(neo) != 2723.33 || youOwe() != 2724.33 {
		t.Fatalf("after borrowing: kotak %v neo %v owe %v", balance(kotak), balance(neo), youOwe())
	}

	// He owes the user ₹4,911 from before Sajni; net off leaves him owing the rest.
	_, _, err = lendBeforeTool(ctx, d, uid, map[string]any{"borrower": "Papa", "amount": 4911.0, "account_id": float64(kotak), "date": "2026-11-01"})
	must(err)
	if balance(kotak) != 1 {
		t.Fatalf("a lend from before Sajni moved money: kotak %v", balance(kotak))
	}
	data, _, err := netOffTool(ctx, d, uid, map[string]any{"person": "Papa"})
	must(err)
	if got := data.(map[string]any)["netted"].(float64); got != 2724.33 || youOwe() != 0 {
		t.Fatalf("net off %v, still owe %v", got, youOwe())
	}

	// Undo the net-off by its lend half, then pay back ₹1,000 from Kotak.
	var lendHalf int64
	must(d.QueryRow(`SELECT t.id FROM fin_transactions t JOIN fin_lend_settlements s ON s.transaction_id = t.id WHERE t.user_id = $1 AND t.type = 'netoff'`, uid).Scan(&lendHalf))
	_, _, err = undoOwedTool(ctx, d, uid, map[string]any{"transaction_id": float64(lendHalf)})
	must(err)
	if youOwe() != 2724.33 {
		t.Fatalf("after undoing the net-off: owe %v", youOwe())
	}
	_, _, err = payBackTool(ctx, d, uid, map[string]any{"person": "Papa", "account_id": float64(kotak), "amount": 1000.0})
	must(err)
	if youOwe() != 1724.33 || balance(kotak) != -999 {
		t.Fatalf("after paying back: owe %v kotak %v", youOwe(), balance(kotak))
	}
	if _, _, err := payBackTool(ctx, d, uid, map[string]any{"person": "Papa", "account_id": float64(kotak), "amount": 5000.0}); err == nil {
		t.Fatal("paying back more than owed was accepted")
	}
	var spend float64
	d.QueryRow(`SELECT COALESCE(SUM(`+db.Spend("")+`),0) FROM fin_transactions WHERE user_id = $1 AND type IN `+db.SpendTypes, uid).Scan(&spend)
	if spend != 0 {
		t.Fatalf("paying back counted as spending: %v", spend)
	}
}
