package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// A payment into an investment (an RD instalment, a SIP) is a Transfer whose
// "to" side is the investment: one transfer_out row on the paying account
// with investment_id set, and no transfer_in leg, because an investment is
// not an account. It leaves the account's balance, adds to the investment's
// invested amount, and is never spending (transfers aren't SpendTypes).
// The API, the auto-debit cron and the AI tools all go through here.

// ErrInvestmentNotFound is returned when the investment isn't the user's.
var ErrInvestmentNotFound = errors.New("investment not found")

// TransferToInvestment records amount moving from accountID into
// investmentID and returns the transaction id. slateID 0 files it on Plain.
func TransferToInvestment(ctx context.Context, tx *sql.Tx, uid string, accountID, investmentID int64,
	amount float64, description, note string, at time.Time, slateID int64,
) (int64, error) {
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM fin_investments WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		investmentID, uid).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrInvestmentNotFound
		}
		return 0, err
	}
	if description == "" {
		description = name
	}
	var slate any
	if slateID != 0 {
		slate = slateID
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions
		(user_id, account_id, type, amount, description, note, txn_at, investment_id, slate_id)
		VALUES ($1,$2,'transfer_out',$3,$4,$5,$6,$7,$8) RETURNING id`,
		uid, accountID, amount, description, note, at, investmentID, slate).Scan(&id); err != nil {
		return 0, err
	}
	return id, ShiftInvested(ctx, tx, uid, investmentID, amount)
}

// ShiftInvested moves an investment's invested amount (and its current value,
// which market-linked ones keep by hand) by delta.
func ShiftInvested(ctx context.Context, q Execer, uid string, investmentID int64, delta float64) error {
	if delta == 0 {
		return nil
	}
	_, err := q.ExecContext(ctx, `UPDATE fin_investments
		SET invested_amount = invested_amount + $1, current_value = current_value + $1, last_updated = NOW()
		WHERE id = $2 AND user_id = $3`, delta, investmentID, uid)
	return err
}

// InvestmentTransfer reports the investment a transaction pays into and its
// amount; ok is false for anything else. Read it before an edit or delete,
// then pass the old amount to FollowInvestmentTransfer.
func InvestmentTransfer(ctx context.Context, q Execer, uid string, txnID int64) (investmentID int64, amount float64, ok bool, err error) {
	var inv sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT investment_id, amount FROM fin_transactions WHERE id = $1 AND user_id = $2`,
		txnID, uid).Scan(&inv, &amount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil || !inv.Valid {
		return 0, 0, false, err
	}
	return inv.Int64, amount, true, nil
}

// FollowInvestmentTransfer keeps the invested amount in step after a payment
// into an investment was edited (its amount may have changed) or deleted
// (the row is gone, so all of it comes back out).
func FollowInvestmentTransfer(ctx context.Context, q Execer, uid string, txnID, investmentID int64, oldAmount float64) error {
	var now float64
	err := q.QueryRowContext(ctx, `SELECT amount FROM fin_transactions WHERE id = $1 AND user_id = $2`, txnID, uid).Scan(&now)
	if errors.Is(err, sql.ErrNoRows) {
		now = 0
	} else if err != nil {
		return err
	}
	return ShiftInvested(ctx, q, uid, investmentID, now-oldAmount)
}
