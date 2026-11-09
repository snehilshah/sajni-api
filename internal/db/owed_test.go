package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/db
func TestOwedFlow(t *testing.T) {
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
	uid := "01900000-0000-7000-8000-0000000000a7"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	must(exec(d, `INSERT INTO users (id, email, name) VALUES ($1, 'owed@test', 'T')`, uid))
	must(exec(d, `INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid))
	var bank, plain int64
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, opening_balance) VALUES ($1,'Bank','savings',1000) RETURNING id`, uid).Scan(&bank))
	must(d.QueryRow(`SELECT id FROM fin_slates WHERE user_id = $1`, uid).Scan(&plain))
	txn := func(typ string, amount float64, at string) int64 {
		t.Helper()
		var id int64
		must(d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, slate_id) VALUES ($1,$2,$3,$4,'x',$5,$6) RETURNING id`,
			uid, bank, typ, amount, at, plain).Scan(&id))
		return id
	}
	balance := func() (v float64) {
		must(d.QueryRow(`SELECT a.opening_balance + COALESCE((SELECT SUM(`+BalanceDelta("t")+`) FROM fin_transactions t WHERE t.account_id = a.id),0)
			FROM fin_accounts a WHERE a.id = $1`, bank).Scan(&v))
		return
	}
	income := func() (v float64) {
		must(d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND type = 'income'`, uid).Scan(&v))
		return
	}
	spend := func() (v float64) {
		must(d.QueryRow(`SELECT COALESCE(SUM(`+Spend("")+`),0) FROM fin_transactions WHERE user_id = $1 AND type IN `+SpendTypes, uid).Scan(&v))
		return
	}
	owed := func(p string) float64 {
		t.Helper()
		v, err := OwedTo(ctx, d, uid, p)
		must(err)
		return v
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
	owedID := func(txnID int64) (id int64) {
		must(d.QueryRow(`SELECT id FROM fin_owed WHERE transaction_id = $1`, txnID).Scan(&id))
		return
	}

	// Papa sends ₹1,00,401; the user owes it back. Money in, not income.
	gift := txn("income", 100401, "2026-11-02T14:17:00+05:30")
	due := "2026-12-31"
	must(inTx(func(tx *sql.Tx) error { return Borrow(ctx, tx, uid, "Papa", gift, &due) }))
	if balance() != 101401 || income() != 0 || owed("papa ") != 100401 {
		t.Fatalf("after borrow: balance %v income %v owed %v", balance(), income(), owed("Papa"))
	}

	// Paying back ₹1,00,000: money out, not spending.
	back := txn("expense", 100000, "2026-11-20T10:00:00+05:30")
	must(inTx(func(tx *sql.Tx) error { return PayBack(ctx, tx, uid, "papa", back) }))
	if balance() != 1401 || spend() != 0 || owed("Papa") != 401 {
		t.Fatalf("after pay back: balance %v spend %v owed %v", balance(), spend(), owed("Papa"))
	}
	var who string
	must(d.QueryRow(`SELECT person FROM fin_owed WHERE transaction_id = $1`, back).Scan(&who))
	if who != "Papa" {
		t.Fatalf("payment filed under %q, want the debt's spelling", who)
	}

	// Paying back more than is owed is refused, and so is paying someone
	// the user never borrowed from.
	over := txn("expense", 500, "2026-11-21T10:00:00+05:30")
	if err := inTx(func(tx *sql.Tx) error { return PayBack(ctx, tx, uid, "Papa", over) }); !errors.Is(err, ErrLendInput) {
		t.Fatalf("overpaying: %v", err)
	}
	if err := inTx(func(tx *sql.Tx) error { return PayBack(ctx, tx, uid, "Nobody", over) }); !errors.Is(err, ErrLendInput) {
		t.Fatalf("paying a stranger back: %v", err)
	}

	// Undoing the borrow would leave Papa paid back more than owed: refused.
	if err := inTx(func(tx *sql.Tx) error { return Unowe(ctx, tx, uid, owedID(gift)) }); !errors.Is(err, ErrLendInput) {
		t.Fatalf("undoing a paid-down borrow: %v", err)
	}
	// Deleting the debt's transaction is refused the same way.
	err = inTx(func(tx *sql.Tx) error { return DeleteTransaction(ctx, tx, uid, gift) })
	var conflict *TxnConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("deleting a paid-down borrow: %v", err)
	}

	// Undo the payment: it's an expense again.
	must(inTx(func(tx *sql.Tx) error { return Unowe(ctx, tx, uid, owedID(back)) }))
	var typ string
	must(d.QueryRow(`SELECT type FROM fin_transactions WHERE id = $1`, back).Scan(&typ))
	if typ != "expense" || owed("Papa") != 100401 {
		t.Fatalf("after undoing the payment: %s, owed %v", typ, owed("Papa"))
	}

	// A debt from before Sajni moves no money.
	before := balance()
	var old int64
	must(inTx(func(tx *sql.Tx) (err error) {
		old, err = AddDebt(ctx, tx, uid, OwedDebt{Person: "Rahul", AccountID: bank, Amount: 300, At: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), SlateID: plain, NoMoneyMoved: true})
		return
	}))
	if balance() != before || owed("Rahul") != 300 {
		t.Fatalf("owed from before: balance %v (was %v), owed %v", balance(), before, owed("Rahul"))
	}

	// Net off: Rahul also owes the user ₹120. Both sides drop by 120, no money moves.
	lendTxn := txn("lend", 120, "2026-10-05T10:00:00+05:30")
	must(exec(d, `INSERT INTO fin_lends (user_id, source_account_id, source_transaction_id, borrower, principal, description, lent_at)
		VALUES ($1,$2,$3,'Rahul',120,'Lunch','2026-10-05T10:00:00+05:30')`, uid, bank, lendTxn))
	before = balance()
	var amount float64
	must(inTx(func(tx *sql.Tx) (err error) {
		amount, err = NetOff(ctx, tx, uid, "rahul", time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
		return
	}))
	theyOwe, _, err := lendTotal(ctx, d, uid, "Rahul")
	must(err)
	if amount != 120 || theyOwe != 0 || owed("Rahul") != 180 || balance() != before {
		t.Fatalf("net off: amount %v they owe %v I owe %v balance %v (was %v)", amount, theyOwe, owed("Rahul"), balance(), before)
	}
	if err := inTx(func(tx *sql.Tx) (err error) {
		_, err = NetOff(ctx, tx, uid, "Rahul", time.Now())
		return
	}); !errors.Is(err, ErrLendInput) {
		t.Fatalf("second net off: %v", err)
	}

	// Undoing it from the lend side removes both halves.
	var settlement int64
	must(d.QueryRow(`SELECT s.id FROM fin_lend_settlements s JOIN fin_transactions t ON t.id = s.transaction_id WHERE t.type = 'netoff' AND s.user_id = $1`, uid).Scan(&settlement))
	must(inTx(func(tx *sql.Tx) error { return Unsettle(ctx, tx, uid, settlement) }))
	var left int
	must(d.QueryRow(`SELECT COUNT(*) FROM fin_transactions WHERE user_id = $1 AND type = 'netoff'`, uid).Scan(&left))
	theyOwe, _, _ = lendTotal(ctx, d, uid, "Rahul")
	if left != 0 || theyOwe != 120 || owed("Rahul") != 300 {
		t.Fatalf("undo from lends: %d netoff rows, they owe %v, I owe %v", left, theyOwe, owed("Rahul"))
	}

	// Net off again and undo it from the debt side.
	must(inTx(func(tx *sql.Tx) (err error) {
		_, err = NetOff(ctx, tx, uid, "Rahul", time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
		return
	}))
	var half int64
	must(d.QueryRow(`SELECT o.id FROM fin_owed o JOIN fin_transactions t ON t.id = o.transaction_id WHERE t.type = 'netoff' AND o.user_id = $1`, uid).Scan(&half))
	must(inTx(func(tx *sql.Tx) error { return Unowe(ctx, tx, uid, half) }))
	must(d.QueryRow(`SELECT COUNT(*) FROM fin_transactions WHERE user_id = $1 AND type = 'netoff'`, uid).Scan(&left))
	if left != 0 || owed("Rahul") != 300 {
		t.Fatalf("undo from debts: %d netoff rows, I owe %v", left, owed("Rahul"))
	}

	// The debt that moved no money is deleted outright when undone.
	must(inTx(func(tx *sql.Tx) error { return Unowe(ctx, tx, uid, owedID(old)) }))
	var gone bool
	must(d.QueryRow(`SELECT NOT EXISTS (SELECT 1 FROM fin_transactions WHERE id = $1)`, old).Scan(&gone))
	if !gone || owed("Rahul") != 0 {
		t.Fatalf("undo an owed row: gone %v, owed %v", gone, owed("Rahul"))
	}
}
