package db

import (
	"context"
	"database/sql"
	"math"
	"os"
	"testing"
	"time"
)

// A bill shared with several people: one share row and lend each, the bill
// keeps the user's part, and the card always shows the whole charge.
// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/db
func TestSplitBillSeveral(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := New(dsn)
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
	uid := "01900000-0000-7000-8000-000000000005"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	must(exec(d, `INSERT INTO users (id, email, name) VALUES ($1, 'split3@test', 'T')`, uid))
	must(exec(d, `INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid))
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var card int64
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, statement_day, due_day) VALUES ($1,'RuPay','credit_card',5,20) RETURNING id`, uid).Scan(&card))
	txn := func(amount float64) int64 {
		var id int64
		must(d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, slate_id)
			VALUES ($1,$2,'expense',$3,'Dinner','2026-10-01T15:00:00Z',(SELECT id FROM fin_slates WHERE user_id=$1)) RETURNING id`,
			uid, card, amount).Scan(&id))
		return id
	}
	inTx := func(f func(tx *sql.Tx) error) error {
		tx, err := d.BeginTx(ctx, nil)
		must(err)
		defer tx.Rollback()
		if err := f(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	val := func(q string, args ...any) (v float64) {
		t.Helper()
		must(d.QueryRow(q, args...).Scan(&v))
		return math.Round(v*100) / 100
	}
	cardTotal := func() float64 {
		return val(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE account_id=$1 AND type IN ('expense','lend')`, card)
	}
	spent := func() float64 {
		return val(`SELECT COALESCE(SUM(`+Spend("")+`),0) FROM fin_transactions WHERE user_id=$1 AND type IN `+SpendTypes, uid)
	}
	owes := func(who string) float64 {
		return val(`SELECT COALESCE(SUM(principal),0) FROM fin_lends WHERE user_id=$1 AND borrower=$2`, uid, who)
	}
	expect := func(label string, got, want float64) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: got %.2f, want %.2f", label, got, want)
		}
	}
	split := func(bill int64, shares ...SplitShare) ([]int64, error) {
		var ids []int64
		err := inTx(func(tx *sql.Tx) error {
			var err error
			ids, err = SplitBill(ctx, tx, uid, bill, shares, nil, false, time.UTC)
			return err
		})
		return ids, err
	}

	// Three ways: you, Rahul and Priya, ₹300 each.
	bill := txn(900)
	ids, err := split(bill, SplitShare{"Rahul", 300}, SplitShare{"Priya", 300})
	must(err)
	if len(ids) != 2 {
		t.Fatalf("want a lend per person, got %d", len(ids))
	}
	expect("3-way: card", cardTotal(), 900)
	expect("3-way: your part", val(`SELECT amount FROM fin_transactions WHERE id=$1`, bill), 300)
	expect("3-way: spent", spent(), 300)
	expect("3-way: Rahul", owes("Rahul"), 300)
	expect("3-way: Priya", owes("Priya"), 300)
	var due sql.NullString
	must(d.QueryRow(`SELECT due_date::text FROM fin_lends WHERE id=$1`, ids[0]).Scan(&due))
	if due.String != "2026-10-20" {
		t.Fatalf("card share should default to the cycle's due date, got %q", due.String)
	}

	// A person can't be in it twice, and shares can't pass the user's part.
	if _, err := split(bill, SplitShare{"rahul", 50}); err == nil {
		t.Fatal("Rahul already has a share")
	}
	if _, err := split(bill, SplitShare{"Aman", 301}); err == nil {
		t.Fatal("a share above the user's part must be refused")
	}
	// Adding someone later takes from the user's part.
	_, err = split(bill, SplitShare{"Aman", 100})
	must(err)
	expect("add Aman: your part", val(`SELECT amount FROM fin_transactions WHERE id=$1`, bill), 200)
	expect("add Aman: card", cardTotal(), 900)

	// Moving one person's line only touches the user's part; with others on
	// the bill it never folds into one lend, even at ₹0 left for the user.
	must(inTx(func(tx *sql.Tx) error { return SetPaidForShare(ctx, tx, uid, ids[0], 500) }))
	expect("Rahul 500: your part", val(`SELECT amount FROM fin_transactions WHERE id=$1`, bill), 0)
	expect("Rahul 500: card", cardTotal(), 900)
	expect("Rahul 500: spent", spent(), 0)
	var billType string
	must(d.QueryRow(`SELECT type FROM fin_transactions WHERE id=$1`, bill).Scan(&billType))
	if billType != "expense" {
		t.Fatalf("a bill shared by several stays the user's (₹0) expense, got %s", billType)
	}
	if err := inTx(func(tx *sql.Tx) error { return SetPaidForShare(ctx, tx, uid, ids[0], 501) }); err == nil {
		t.Fatal("a share above the bill less the others' must be refused")
	}

	// Removing one person folds their share back into the user's part.
	must(inTx(func(tx *sql.Tx) error { return UnmarkPaidFor(ctx, tx, uid, ids[1]) }))
	expect("Priya out: your part", val(`SELECT amount FROM fin_transactions WHERE id=$1`, bill), 300)
	expect("Priya out: card", cardTotal(), 900)
	expect("Priya out: Priya", owes("Priya"), 0)

	// The bill's date moves every share with it.
	must(exec(d, `UPDATE fin_transactions SET txn_at='2026-10-02T15:00:00Z' WHERE id=$1`, bill))
	must(inTx(func(tx *sql.Tx) error { return SyncSplit(ctx, tx, uid, bill) }))
	var moved int
	must(d.QueryRow(`SELECT COUNT(*) FROM fin_lends WHERE user_id=$1 AND lent_at='2026-10-02T15:00:00Z'`, uid).Scan(&moved))
	if moved != 2 {
		t.Fatalf("both remaining shares should follow the bill's date, got %d", moved)
	}

	// Paid for both friends entirely: the user's part is ₹0.
	tickets := txn(600)
	_, err = split(tickets, SplitShare{"Rahul", 300}, SplitShare{"Priya", 300})
	must(err)
	expect("tickets: your part", val(`SELECT amount FROM fin_transactions WHERE id=$1`, tickets), 0)
	expect("tickets: card", cardTotal(), 1500)

	// One person on a fresh bill keeps the old path: All makes the bill their lend.
	lend := txn(200)
	_, err = split(lend, SplitShare{"Dad", 200})
	must(err)
	must(d.QueryRow(`SELECT type FROM fin_transactions WHERE id=$1`, lend).Scan(&billType))
	if billType != "lend" {
		t.Fatalf("one person, all of it: the bill becomes the lend, got %s", billType)
	}
}
