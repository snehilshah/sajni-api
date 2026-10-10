package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Lends are whole transactions. "Paid for" turns an existing expense into a
// lend (origin 'paid_for'); "settle" turns an existing credit into a
// settlement for a person. Settlement credits are pooled per person and
// allocated oldest-lend-first by Rebalance, so a single transfer can clear
// several lends and any surplus waits for that person's next lend.

var ErrLendInput = errors.New("lend input")

func lendErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLendInput, fmt.Sprintf(format, args...))
}

const borrowerMatch = "LOWER(BTRIM(borrower)) = LOWER(BTRIM($2))"

// CardCycleDue is the due date of the card statement that will carry a charge
// made on `at`: the next statement day on/after the charge, then its due day.
// Days clamp to 28 (same rule as statement due-date derivation).
func CardCycleDue(at time.Time, statementDay, dueDay int) time.Time {
	sd := min(max(statementDay, 1), 28)
	stmt := time.Date(at.Year(), at.Month(), sd, 0, 0, 0, 0, time.UTC)
	if at.Day() > sd {
		stmt = stmt.AddDate(0, 1, 0)
	}
	dd := dueDay
	if dd <= 0 {
		dd = sd + 15
	}
	due := time.Date(stmt.Year(), stmt.Month(), 1, 0, 0, 0, 0, time.UTC)
	if dd <= sd {
		due = due.AddDate(0, 1, 0)
	}
	return due.AddDate(0, 0, min(dd, 28)-1)
}

// MarkPaidFor converts expenses into lends to borrower. dueDate nil = derive
// from the card cycle (cards only); "" = no due date.
func MarkPaidFor(ctx context.Context, tx *sql.Tx, uid, borrower string, txnIDs []int64, dueDate *string, remind bool, loc *time.Location) ([]int64, error) {
	borrower = strings.TrimSpace(borrower)
	if borrower == "" || len(txnIDs) == 0 {
		return nil, lendErr("Pick a person and at least one transaction.")
	}
	var ids []int64
	for _, txnID := range txnIDs {
		var typ, accountType, description, note string
		var amount float64
		var accountID int64
		var at time.Time
		var transferPair sql.NullInt64
		var statementDay, dueDay sql.NullInt64
		var invested bool
		err := tx.QueryRowContext(ctx, `SELECT t.type, t.amount, t.account_id, t.description, t.note, t.txn_at, t.transfer_pair,
			a.type, a.statement_day, a.due_day,
			EXISTS (SELECT 1 FROM fin_investment_contributions c WHERE c.txn_id = t.id)
			FROM fin_transactions t JOIN fin_accounts a ON a.id = t.account_id
			WHERE t.id = $1 AND t.user_id = $2 FOR UPDATE OF t`, txnID, uid,
		).Scan(&typ, &amount, &accountID, &description, &note, &at, &transferPair, &accountType, &statementDay, &dueDay, &invested)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, lendErr("A selected transaction no longer exists (#%d).", txnID)
		}
		if err != nil {
			return nil, err
		}
		if typ != "expense" || transferPair.Valid || invested {
			return nil, lendErr("Only plain expenses can be marked paid for (#%d is already a lend, transfer or income).", txnID)
		}
		var due any
		switch {
		case dueDate != nil && *dueDate != "":
			due = *dueDate
		case dueDate == nil && accountType == "credit_card" && statementDay.Valid:
			due = CardCycleDue(at.In(loc), int(statementDay.Int64), int(dueDay.Int64)).Format("2006-01-02")
		}
		if description == "" {
			description = "Paid for " + borrower
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'lend', updated_at = NOW() WHERE id = $1 AND user_id = $2`, txnID, uid); err != nil {
			return nil, err
		}
		var lendID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_lends
			(user_id, source_account_id, source_transaction_id, borrower, principal, description, note, lent_at, due_date, remind, origin)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'paid_for') RETURNING id`,
			uid, accountID, txnID, borrower, amount, description, note, at, due, remind && due != nil,
		).Scan(&lendID); err != nil {
			return nil, err
		}
		ids = append(ids, lendID)
	}
	return ids, RebalanceLends(ctx, tx, uid, borrower)
}

// SettleWith converts credits into settlements from borrower.
func SettleWith(ctx context.Context, tx *sql.Tx, uid, borrower string, txnIDs []int64) error {
	borrower = strings.TrimSpace(borrower)
	if borrower == "" || len(txnIDs) == 0 {
		return lendErr("Pick a person and at least one transaction.")
	}
	var known bool
	tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+`)`, uid, borrower).Scan(&known)
	if !known {
		return lendErr("%s doesn't owe anything right now.", borrower)
	}
	for _, txnID := range txnIDs {
		var typ string
		var transferPair sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT type, transfer_pair FROM fin_transactions WHERE id = $1 AND user_id = $2 FOR UPDATE`, txnID, uid).Scan(&typ, &transferPair)
		if errors.Is(err, sql.ErrNoRows) {
			return lendErr("A selected transaction no longer exists (#%d).", txnID)
		}
		if err != nil {
			return err
		}
		if typ != "income" || transferPair.Valid {
			return lendErr("Only plain income can settle a lend (#%d is already linked to something else).", txnID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'lend_repayment', updated_at = NOW() WHERE id = $1 AND user_id = $2`, txnID, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO fin_lend_settlements (user_id, borrower, transaction_id) VALUES ($1,$2,$3)`, uid, borrower, txnID); err != nil {
			return err
		}
	}
	return RebalanceLends(ctx, tx, uid, borrower)
}

// RecordSettlement books money received that has no transaction yet (cash,
// an uncaptured transfer) and settles it in one step.
func RecordSettlement(ctx context.Context, tx *sql.Tx, uid, borrower string, accountID int64, amount float64, at time.Time, note string) (int64, error) {
	borrower = strings.TrimSpace(borrower)
	if borrower == "" || accountID == 0 || amount <= 0 {
		return 0, lendErr("Enter who it's from, an account and an amount above zero.")
	}
	var txnID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, note, txn_at)
		SELECT $1, id, 'income', $3, $4, $5, $6 FROM fin_accounts WHERE id = $2 AND user_id = $1 RETURNING id`,
		uid, accountID, math.Round(amount*100)/100, "Received from "+borrower, strings.TrimSpace(note), at,
	).Scan(&txnID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, lendErr("That account no longer exists.")
		}
		return 0, err
	}
	return txnID, SettleWith(ctx, tx, uid, borrower, []int64{txnID})
}

// Unsettle turns a settlement back into a plain credit.
func Unsettle(ctx context.Context, tx *sql.Tx, uid string, settlementID int64) error {
	var borrower string
	var txnID int64
	err := tx.QueryRowContext(ctx, `DELETE FROM fin_lend_settlements WHERE id = $1 AND user_id = $2 RETURNING borrower, transaction_id`, settlementID, uid).Scan(&borrower, &txnID)
	if errors.Is(err, sql.ErrNoRows) {
		return lendErr("That settlement no longer exists.")
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fin_lend_repayments WHERE user_id = $1 AND transaction_id = $2`, uid, txnID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'income', updated_at = NOW() WHERE id = $1 AND user_id = $2`, txnID, uid); err != nil {
		return err
	}
	return RebalanceLends(ctx, tx, uid, borrower)
}

// UnmarkPaidFor turns a paid-for lend back into the expense it came from.
// Repayments recorded directly against it stay as received money (income).
func UnmarkPaidFor(ctx context.Context, tx *sql.Tx, uid string, lendID int64) error {
	var borrower, origin string
	var txnID int64
	err := tx.QueryRowContext(ctx, `SELECT borrower, origin, source_transaction_id FROM fin_lends WHERE id = $1 AND user_id = $2 FOR UPDATE`, lendID, uid).Scan(&borrower, &origin, &txnID)
	if errors.Is(err, sql.ErrNoRows) {
		return lendErr("That lend no longer exists.")
	}
	if err != nil {
		return err
	}
	if origin != "paid_for" {
		return lendErr("This lend wasn't made from a transaction, so there's nothing to undo. Delete it instead.")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'income', updated_at = NOW()
		WHERE user_id = $1 AND id IN (
			SELECT r.transaction_id FROM fin_lend_repayments r
			WHERE r.lend_id = $2 AND NOT EXISTS (SELECT 1 FROM fin_lend_settlements s WHERE s.transaction_id = r.transaction_id))`, uid, lendID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fin_lends WHERE id = $1 AND user_id = $2`, lendID, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'expense', updated_at = NOW() WHERE id = $1 AND user_id = $2`, txnID, uid); err != nil {
		return err
	}
	return RebalanceLends(ctx, tx, uid, borrower)
}

// RebalanceLends re-derives how a person's settlement credits cover their
// lends: oldest credit to oldest lend, after repayments recorded directly
// against a lend. Deterministic, so it runs after every change.
func RebalanceLends(ctx context.Context, tx *sql.Tx, uid, borrower string) error {
	type lend struct {
		id   int64
		room float64
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, principal FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+` ORDER BY lent_at, id FOR UPDATE`, uid, borrower)
	if err != nil {
		return err
	}
	var lends []lend
	for rows.Next() {
		var l lend
		if err := rows.Scan(&l.id, &l.room); err != nil {
			rows.Close()
			return err
		}
		lends = append(lends, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM fin_lend_repayments r USING fin_lend_settlements s
		WHERE r.user_id = $1 AND s.user_id = $1 AND r.transaction_id = s.transaction_id AND LOWER(BTRIM(s.borrower)) = LOWER(BTRIM($2))`, uid, borrower); err != nil {
		return err
	}
	for i := range lends {
		var direct float64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount),0) FROM fin_lend_repayments WHERE lend_id = $1`, lends[i].id).Scan(&direct); err != nil {
			return err
		}
		lends[i].room = math.Max(lends[i].room-direct, 0)
	}

	credits, err := tx.QueryContext(ctx, `SELECT t.id, t.account_id, t.amount, t.txn_at FROM fin_lend_settlements s
		JOIN fin_transactions t ON t.id = s.transaction_id
		WHERE s.user_id = $1 AND LOWER(BTRIM(s.borrower)) = LOWER(BTRIM($2)) ORDER BY t.txn_at, t.id`, uid, borrower)
	if err != nil {
		return err
	}
	type credit struct {
		txnID, accountID int64
		amount           float64
		at               time.Time
	}
	var pool []credit
	for credits.Next() {
		var c credit
		if err := credits.Scan(&c.txnID, &c.accountID, &c.amount, &c.at); err != nil {
			credits.Close()
			return err
		}
		pool = append(pool, c)
	}
	credits.Close()
	if err := credits.Err(); err != nil {
		return err
	}

	li := 0
	for _, c := range pool {
		left := c.amount
		for left > 0.004 && li < len(lends) {
			if lends[li].room <= 0.004 {
				li++
				continue
			}
			part := math.Round(math.Min(left, lends[li].room)*100) / 100
			if _, err := tx.ExecContext(ctx, `INSERT INTO fin_lend_repayments
				(user_id, lend_id, destination_account_id, transaction_id, amount, repaid_at) VALUES ($1,$2,$3,$4,$5,$6)`,
				uid, lends[li].id, c.accountID, c.txnID, part, c.at); err != nil {
				return err
			}
			lends[li].room -= part
			left -= part
		}
	}
	return nil
}
