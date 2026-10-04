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

	// Upgrade path: an old database still has the unique constraint.
	d.Exec(`ALTER TABLE fin_lend_repayments ADD CONSTRAINT fin_lend_repayments_transaction_id_key UNIQUE (transaction_id)`)
	must(d.ensureSchema())

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
		_, err := MarkPaidFor(ctx, tx, uid, "Dad", []int64{elec, shop}, nil, false, time.UTC)
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
		_, err := MarkPaidFor(ctx, tx, uid, "Dad", []int64{more}, nil, false, time.UTC)
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

func exec(d *DB, q string, args ...any) error {
	_, err := d.Exec(q, args...)
	return err
}
