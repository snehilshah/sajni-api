package db

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/db
func TestTransferToInvestment(t *testing.T) {
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
	inTx := func(f func(tx *sql.Tx) error) {
		t.Helper()
		tx, err := d.BeginTx(ctx, nil)
		must(err)
		defer tx.Rollback()
		must(f(tx))
		must(tx.Commit())
	}

	uid := "01900000-0000-7000-8000-000000000002"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	must(exec(d, `INSERT INTO users (id, email, name) VALUES ($1, 'invest@test', 'T')`, uid))
	must(exec(d, `INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid))
	var bank, rd int64
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, opening_balance) VALUES ($1,'Bank','savings',50000) RETURNING id`, uid).Scan(&bank))
	must(d.QueryRow(`INSERT INTO fin_investments (user_id, name, type, invested_amount, current_value, monthly_amount) VALUES ($1,'Savings RD','rd',130000,130000,10000) RETURNING id`, uid).Scan(&rd))
	invested := func() (v float64) {
		d.QueryRow(`SELECT invested_amount FROM fin_investments WHERE id=$1`, rd).Scan(&v)
		return
	}

	var id int64
	inTx(func(tx *sql.Tx) (err error) {
		id, err = TransferToInvestment(ctx, tx, uid, bank, rd, 10000, "", "", time.Now(), 0)
		return
	})
	var typ, title string
	var spend float64
	must(d.QueryRow(`SELECT type, description FROM fin_transactions WHERE id=$1`, id).Scan(&typ, &title))
	if typ != "transfer_out" || title != "Savings RD" {
		t.Fatalf("got %s %q, want a transfer_out titled after the investment", typ, title)
	}
	must(d.QueryRow(`SELECT COALESCE(SUM(`+Spend("")+`),0) FROM fin_transactions WHERE user_id=$1 AND type IN `+SpendTypes, uid).Scan(&spend))
	if spend != 0 {
		t.Fatalf("a payment into an investment counted as spending: %v", spend)
	}
	if got := invested(); got != 140000 {
		t.Fatalf("invested %v after paying in, want 140000", got)
	}

	// Edit the amount: the invested amount follows.
	inTx(func(tx *sql.Tx) error {
		inv, old, ok, err := InvestmentTransfer(ctx, tx, uid, id)
		if err != nil || !ok {
			t.Fatalf("not seen as an investment transfer: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET amount=12000 WHERE id=$1`, id); err != nil {
			return err
		}
		return FollowInvestmentTransfer(ctx, tx, uid, id, inv, old)
	})
	if got := invested(); got != 142000 {
		t.Fatalf("invested %v after edit, want 142000", got)
	}

	// Delete: all of it comes back out.
	inTx(func(tx *sql.Tx) error {
		inv, old, _, err := InvestmentTransfer(ctx, tx, uid, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id=$1`, id); err != nil {
			return err
		}
		return FollowInvestmentTransfer(ctx, tx, uid, id, inv, old)
	})
	if got := invested(); got != 130000 {
		t.Fatalf("invested %v after delete, want 130000", got)
	}

	// Someone else's investment is refused.
	tx, err := d.BeginTx(ctx, nil)
	must(err)
	defer tx.Rollback()
	if _, err := TransferToInvestment(ctx, tx, "01900000-0000-7000-8000-000000000003", bank, rd, 1, "", "", time.Now(), 0); err != ErrInvestmentNotFound {
		t.Fatalf("got %v, want ErrInvestmentNotFound", err)
	}
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
}
