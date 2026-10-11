package db

import (
	"context"
	"database/sql"
	"math"
	"os"
	"testing"
	"time"
)

func TestCardCycleDue(t *testing.T) {
	cases := []struct{ at, want string }{
		{"2026-08-05", "2026-09-20"}, // paid after the 1 Aug statement → Sep statement
		{"2026-08-01", "2026-08-20"}, // on statement day → that statement
		{"2026-12-15", "2027-01-20"},
	}
	for _, c := range cases {
		at, _ := time.Parse("2006-01-02", c.at)
		if got := CardCycleDue(at, 1, 20).Format("2006-01-02"); got != c.want {
			t.Errorf("CardCycleDue(%s) = %s, want %s", c.at, got, c.want)
		}
	}
	at, _ := time.Parse("2006-01-02", "2026-08-10")
	if got := CardCycleDue(at, 15, 5).Format("2006-01-02"); got != "2026-09-05" {
		t.Errorf("due before statement day should roll a month: %s", got)
	}
}

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/db
func TestPaidForSettleFlow(t *testing.T) {
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

	uid := "01900000-0000-7000-8000-000000000001"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	must(exec(d, `INSERT INTO users (id, email, name) VALUES ($1, 'lends@test', 'T')`, uid))
	must(exec(d, `INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid))
	var card, bank int64
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, statement_day, due_day) VALUES ($1,'Card','credit_card',1,20) RETURNING id`, uid).Scan(&card))
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Bank','savings') RETURNING id`, uid).Scan(&bank))
	txn := func(acct int64, typ string, amount float64, at string) int64 {
		var id int64
		must(d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at) VALUES ($1,$2,$3,$4,'x',$5) RETURNING id`,
			uid, acct, typ, amount, at+"T10:00:00Z").Scan(&id))
		return id
	}
	inTx := func(f func(tx *sql.Tx) error) {
		t.Helper()
		tx, err := d.BeginTx(ctx, nil)
		must(err)
		defer tx.Rollback()
		must(f(tx))
		must(tx.Commit())
	}
	outstanding := func() (owed, credit float64) {
		d.QueryRow(`SELECT COALESCE(SUM(principal),0) - COALESCE((SELECT SUM(amount) FROM fin_lend_repayments WHERE user_id=$1),0) FROM fin_lends WHERE user_id=$1`, uid).Scan(&owed)
		d.QueryRow(`SELECT COALESCE(SUM(t.amount),0) - COALESCE((SELECT SUM(r.amount) FROM fin_lend_repayments r JOIN fin_lend_settlements s ON s.transaction_id=r.transaction_id),0)
			FROM fin_lend_settlements s JOIN fin_transactions t ON t.id=s.transaction_id WHERE s.user_id=$1`, uid).Scan(&credit)
		return math.Round(owed*100) / 100, math.Round(credit*100) / 100
	}
	typeOf := func(id int64) (s string) {
		d.QueryRow(`SELECT type FROM fin_transactions WHERE id=$1`, id).Scan(&s)
		return
	}
	check := func(label string, wantOwed, wantCredit float64) {
		t.Helper()
		if o, c := outstanding(); o != wantOwed || c != wantCredit {
			t.Fatalf("%s: owed %.2f credit %.2f, want %.2f / %.2f", label, o, c, wantOwed, wantCredit)
		}
	}

	elec := txn(card, "expense", 1840, "2026-08-05")
	shop := txn(card, "expense", 500, "2026-08-10")
	inTx(func(tx *sql.Tx) error {
		_, err := MarkPaidFor(ctx, tx, uid, "Dad", []int64{elec, shop}, nil, false, time.UTC, 0)
		return err
	})
	var due string
	d.QueryRow(`SELECT due_date::text FROM fin_lends WHERE source_transaction_id=$1`, elec).Scan(&due)
	if due != "2026-09-20" || typeOf(elec) != "lend" {
		t.Fatalf("paid for: due %s type %s", due, typeOf(elec))
	}
	check("marked", 2340, 0)

	c1 := txn(bank, "income", 1000, "2026-09-02")
	inTx(func(tx *sql.Tx) error { return SettleWith(ctx, tx, uid, "dad ", []int64{c1}) })
	check("partial settle", 1340, 0)

	c2 := txn(bank, "income", 1500, "2026-09-10")
	inTx(func(tx *sql.Tx) error { return SettleWith(ctx, tx, uid, "Dad", []int64{c2}) })
	check("over settle", 0, 160)

	more := txn(card, "expense", 300, "2026-09-12")
	inTx(func(tx *sql.Tx) error {
		_, err := MarkPaidFor(ctx, tx, uid, "Dad", []int64{more}, nil, false, time.UTC, 0)
		return err
	})
	check("credit carried to new lend", 140, 0)

	var s1 int64
	d.QueryRow(`SELECT id FROM fin_lend_settlements WHERE transaction_id=$1`, c1).Scan(&s1)
	inTx(func(tx *sql.Tx) error { return Unsettle(ctx, tx, uid, s1) })
	if typeOf(c1) != "income" {
		t.Fatal("unsettled credit should be income again")
	}
	check("unsettle", 1140, 0)

	var elecLend int64
	d.QueryRow(`SELECT id FROM fin_lends WHERE source_transaction_id=$1`, elec).Scan(&elecLend)
	inTx(func(tx *sql.Tx) error { return UnmarkPaidFor(ctx, tx, uid, elecLend) })
	if typeOf(elec) != "expense" {
		t.Fatal("unmarked lend should be an expense again")
	}
	check("unmark", 0, 700)

	inTx(func(tx *sql.Tx) error {
		if err := SettleWith(ctx, tx, uid, "Nobody", []int64{c1}); err == nil {
			t.Error("settling for an unknown person should fail")
		}
		return nil
	})
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
}

// Needs a scratch Postgres, like TestPaidForSettleFlow.
func TestSplitAndForgive(t *testing.T) {
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
	uid := "01900000-0000-7000-8000-000000000002"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	must(exec(d, `INSERT INTO users (id, email, name) VALUES ($1, 'split@test', 'T')`, uid))
	must(exec(d, `INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid))
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var card, bank int64
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, statement_day, due_day) VALUES ($1,'RuPay','credit_card',5,20) RETURNING id`, uid).Scan(&card))
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Savings','savings') RETURNING id`, uid).Scan(&bank))
	txn := func(acct int64, typ string, amount float64, at string) int64 {
		var id int64
		must(d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at) VALUES ($1,$2,$3,$4,'Dinner',$5) RETURNING id`,
			uid, acct, typ, amount, at+"T15:00:00Z").Scan(&id))
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
	sum := func(q string, args ...any) (v float64) {
		t.Helper()
		must(d.QueryRow(q, args...).Scan(&v))
		return math.Round(v*100) / 100
	}
	cardTotal := func() float64 {
		return sum(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE account_id=$1 AND type IN ('expense','lend')`, card)
	}
	spent := func() float64 {
		return sum(`SELECT COALESCE(SUM(`+Spend("")+`),0) FROM fin_transactions WHERE user_id=$1 AND type IN `+SpendTypes, uid)
	}
	owed := func() float64 {
		return sum(`SELECT COALESCE(SUM(principal),0) - COALESCE((SELECT SUM(amount) FROM fin_lend_repayments WHERE user_id=$1),0) FROM fin_lends WHERE user_id=$1`, uid)
	}
	expect := func(label string, got, want float64) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: got %.2f, want %.2f", label, got, want)
		}
	}

	bill := txn(card, "expense", 754.96, "2026-10-01")
	var lendID int64
	must(inTx(func(tx *sql.Tx) error {
		ids, err := MarkPaidFor(ctx, tx, uid, "Rahul", []int64{bill}, nil, false, time.UTC, 377.48)
		if len(ids) == 1 {
			lendID = ids[0]
		}
		return err
	}))
	expect("split: card total", cardTotal(), 754.96)
	expect("split: spent", spent(), 377.48)
	expect("split: owed", owed(), 377.48)
	var billType string
	d.QueryRow(`SELECT type FROM fin_transactions WHERE id=$1`, bill).Scan(&billType)
	if billType != "expense" {
		t.Fatalf("bill should stay the user's expense, got %s", billType)
	}
	if err := inTx(func(tx *sql.Tx) error {
		_, err := MarkPaidFor(ctx, tx, uid, "Aman", []int64{bill}, nil, false, time.UTC, 0)
		return err
	}); err == nil {
		t.Fatal("a split bill can't be paid for again")
	}

	// Moving the line keeps the bill whole.
	must(inTx(func(tx *sql.Tx) error { return SetPaidForShare(ctx, tx, uid, lendID, 500) }))
	expect("share 500: card", cardTotal(), 754.96)
	expect("share 500: spent", spent(), 254.96)
	must(inTx(func(tx *sql.Tx) error { return SetPaidForShare(ctx, tx, uid, lendID, 754.96) }))
	expect("all: card", cardTotal(), 754.96)
	expect("all: spent", spent(), 0)
	d.QueryRow(`SELECT type FROM fin_transactions WHERE id=$1`, bill).Scan(&billType)
	if billType != "lend" {
		t.Fatalf("whole share should make the bill the lend, got %s", billType)
	}
	if err := inTx(func(tx *sql.Tx) error { return SetPaidForShare(ctx, tx, uid, lendID, 800) }); err == nil {
		t.Fatal("share above the bill should fail")
	}
	must(inTx(func(tx *sql.Tx) error { return SetPaidForShare(ctx, tx, uid, lendID, 377.48) }))
	expect("half again: card", cardTotal(), 754.96)

	// Bulk repayment into savings clears oldest first.
	cab := txn(bank, "expense", 40, "2026-10-03")
	must(inTx(func(tx *sql.Tx) error {
		_, err := MarkPaidFor(ctx, tx, uid, "Rahul", []int64{cab}, nil, false, time.UTC, 0)
		return err
	}))
	expect("two lends", owed(), 417.48)
	pay := txn(bank, "income", 100, "2026-10-05")
	must(inTx(func(tx *sql.Tx) error { return SettleWith(ctx, tx, uid, "Rahul", []int64{pay}) }))
	expect("after 100", owed(), 317.48)

	// Forgive the rest: spending on that day, no money moves.
	if err := inTx(func(tx *sql.Tx) error {
		_, err := Forgive(ctx, tx, uid, "Rahul", 400, nil, time.Now(), "")
		return err
	}); err == nil {
		t.Fatal("forgiving more than owed should fail")
	}
	var writeoff int64
	must(inTx(func(tx *sql.Tx) error {
		var err error
		writeoff, err = Forgive(ctx, tx, uid, "Rahul", 317.48, nil, time.Now(), "")
		return err
	}))
	expect("forgiven: owed", owed(), 0)
	expect("forgiven: spent", spent(), 377.48+317.48)
	expect("forgiven: card", cardTotal(), 754.96)
	var sid int64
	d.QueryRow(`SELECT id FROM fin_lend_settlements WHERE transaction_id=$1`, writeoff).Scan(&sid)
	must(inTx(func(tx *sql.Tx) error { return Unsettle(ctx, tx, uid, sid) }))
	expect("unforgiven: owed", owed(), 317.48)
	var gone bool
	d.QueryRow(`SELECT NOT EXISTS (SELECT 1 FROM fin_transactions WHERE id=$1)`, writeoff).Scan(&gone)
	if !gone {
		t.Fatal("undoing a forgive should delete the writeoff")
	}

	// Undo the split: the share folds back into the bill.
	must(inTx(func(tx *sql.Tx) error { return UnmarkPaidFor(ctx, tx, uid, lendID) }))
	expect("unsplit: card", cardTotal(), 754.96)
	expect("unsplit: bill", sum(`SELECT amount FROM fin_transactions WHERE id=$1`, bill), 754.96)
	var splits int
	d.QueryRow(`SELECT COUNT(*) FROM fin_transactions WHERE split_of=$1`, bill).Scan(&splits)
	if splits != 0 {
		t.Fatal("split row should be gone")
	}
}

func exec(d *DB, q string, args ...any) error {
	_, err := d.Exec(q, args...)
	return err
}
