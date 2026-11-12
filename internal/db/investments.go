package db

import (
	"context"
	"database/sql"
	"errors"
	"math"
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

// TransferFromInvestment records money coming back from an investment (an
// RD maturing, a SIP redeemed, an FD broken) into accountID. Up to what is
// still invested is the user's own money: one transfer_in row with
// investment_id, lowering the invested amount (never income). Anything above
// it is a gain: an income row (the Interest category) with the same
// investment_id. Once nothing is left invested the investment is closed:
// current value 0, auto-debit off. Returns both ids (interest 0 if none).
func TransferFromInvestment(ctx context.Context, tx *sql.Tx, uid string, accountID, investmentID int64,
	amount float64, description, note string, at time.Time, slateID int64,
) (transferID, interestID int64, err error) {
	var name string
	var invested float64
	if err := tx.QueryRowContext(ctx, `SELECT name, invested_amount FROM fin_investments WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		investmentID, uid).Scan(&name, &invested); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrInvestmentNotFound
		}
		return 0, 0, err
	}
	principal, gain := SplitReturn(invested, amount)
	if description == "" {
		description = name
	}
	var slate any
	if slateID != 0 {
		slate = slateID
	}
	if principal > 0 {
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions
			(user_id, account_id, type, amount, description, note, txn_at, investment_id, slate_id)
			VALUES ($1,$2,'transfer_in',$3,$4,$5,$6,$7,$8) RETURNING id`,
			uid, accountID, principal, description, note, at, investmentID, slate).Scan(&transferID); err != nil {
			return 0, 0, err
		}
		if err := ShiftInvested(ctx, tx, uid, investmentID, -principal); err != nil {
			return 0, 0, err
		}
	}
	if gain > 0 {
		var cat sql.NullInt64
		tx.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND kind = 'income' AND LOWER(BTRIM(name)) = 'interest' ORDER BY id LIMIT 1`, uid).Scan(&cat)
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions
			(user_id, account_id, category_id, type, amount, description, note, txn_at, investment_id, slate_id)
			VALUES ($1,$2,$3,'income',$4,$5,$6,$7,$8,$9) RETURNING id`,
			uid, accountID, cat, gain, name+" interest", "Earned on "+name, at, investmentID, slate).Scan(&interestID); err != nil {
			return 0, 0, err
		}
	}
	if invested-principal < 0.005 {
		if _, err := tx.ExecContext(ctx, `UPDATE fin_investments SET current_value = 0, auto_debit = FALSE, last_updated = NOW()
			WHERE id = $1 AND user_id = $2`, investmentID, uid); err != nil {
			return 0, 0, err
		}
	}
	return transferID, interestID, nil
}

// SplitReturn divides money back from an investment into the user's own
// money (up to what is still invested) and the gain above it.
func SplitReturn(invested, amount float64) (principal, gain float64) {
	principal = math.Min(math.Max(invested, 0), amount)
	principal = math.Round(principal*100) / 100
	return principal, math.Round((amount-principal)*100) / 100
}

// InvestmentTransfer reports the investment a transaction moves money into
// or out of, and its effect on the invested amount (+amount for a payment
// in, -amount for money back, 0 for the gain row); ok is false for anything
// else. Read it before an edit or delete, then pass the old effect to
// FollowInvestmentTransfer.
func InvestmentTransfer(ctx context.Context, q Execer, uid string, txnID int64) (investmentID int64, effect float64, ok bool, err error) {
	var inv sql.NullInt64
	var typ string
	var amount float64
	err = q.QueryRowContext(ctx, `SELECT investment_id, type, amount FROM fin_transactions WHERE id = $1 AND user_id = $2`,
		txnID, uid).Scan(&inv, &typ, &amount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil || !inv.Valid {
		return 0, 0, false, err
	}
	return inv.Int64, investedEffect(typ, amount), true, nil
}

func investedEffect(typ string, amount float64) float64 {
	switch typ {
	case "transfer_out":
		return amount
	case "transfer_in":
		return -amount
	}
	return 0
}

// FollowInvestmentTransfer keeps the invested amount in step after a row
// moving money into or out of an investment was edited (its amount may have
// changed) or deleted (the row is gone, so its whole effect is undone).
func FollowInvestmentTransfer(ctx context.Context, q Execer, uid string, txnID, investmentID int64, oldEffect float64) error {
	var typ string
	var amount float64
	now := 0.0
	err := q.QueryRowContext(ctx, `SELECT type, amount FROM fin_transactions WHERE id = $1 AND user_id = $2`, txnID, uid).Scan(&typ, &amount)
	if err == nil {
		now = investedEffect(typ, amount)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return ShiftInvested(ctx, q, uid, investmentID, now-oldEffect)
}
