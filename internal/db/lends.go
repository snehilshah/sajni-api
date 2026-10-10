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

// "Paid for" turns an existing expense into a lend (origin 'paid_for'). With a
// share below the bill it is a split: the bill keeps the user's part and the
// share is cut into its own lend row (split_of = bill), so the two still sum
// to the charge. "Settle" turns an existing credit into a settlement for a
// person; "forgive" books a writeoff that settles like a credit. Settlement
// credits are pooled per person and allocated oldest-lend-first by Rebalance,
// so a single transfer can clear several lends and any surplus waits for that
// person's next lend.

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
// from the card cycle (cards only); "" = no due date. share > 0 (one
// transaction only) splits the bill: borrower owes share, the rest stays the
// user's expense.
func MarkPaidFor(ctx context.Context, tx *sql.Tx, uid, borrower string, txnIDs []int64, dueDate *string, remind bool, loc *time.Location, share float64) ([]int64, error) {
	borrower = strings.TrimSpace(borrower)
	if borrower == "" || len(txnIDs) == 0 {
		return nil, lendErr("Pick a person and at least one transaction.")
	}
	if share > 0 && len(txnIDs) != 1 {
		return nil, lendErr("A share applies to one bill at a time.")
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
			OR EXISTS (SELECT 1 FROM fin_transactions sp WHERE sp.split_of = t.id)
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
			return nil, lendErr("Only plain expenses can be marked paid for (#%d is already a lend, a split bill, a transfer or income).", txnID)
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
		if share > 0 && roundMoney(share) < roundMoney(amount) {
			if err := SetPaidForShare(ctx, tx, uid, lendID, share); err != nil {
				return nil, err
			}
		}
	}
	return ids, RebalanceLends(ctx, tx, uid, borrower)
}

func roundMoney(v float64) float64 { return math.Round(v*100) / 100 }

// SetPaidForShare moves the line between the user's part of a paid-for bill
// and the borrower's. The bill's total never changes, so the account and its
// card statement stay whole:
//   - share = total: the original row itself is the lend (type 'lend').
//   - share < total: the original row is the user's expense holding the rest,
//     and a split_of row (type 'lend') holds the share.
func SetPaidForShare(ctx context.Context, tx *sql.Tx, uid string, lendID int64, share float64) error {
	share = roundMoney(share)
	var origin, borrower string
	var lendTxn int64
	var direct float64
	err := tx.QueryRowContext(ctx, `SELECT l.origin, l.borrower, l.source_transaction_id,
		COALESCE((SELECT SUM(r.amount) FROM fin_lend_repayments r WHERE r.lend_id = l.id
			AND NOT EXISTS (SELECT 1 FROM fin_lend_settlements s WHERE s.transaction_id = r.transaction_id)),0)
		FROM fin_lends l WHERE l.id = $1 AND l.user_id = $2 FOR UPDATE`, lendID, uid).Scan(&origin, &borrower, &lendTxn, &direct)
	if errors.Is(err, sql.ErrNoRows) {
		return lendErr("That lend no longer exists.")
	}
	if err != nil {
		return err
	}
	if origin != "paid_for" {
		return lendErr("Only a bill you paid for can be split.")
	}
	var lendAmount float64
	var splitOf sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT amount, split_of FROM fin_transactions WHERE id = $1 AND user_id = $2 FOR UPDATE`, lendTxn, uid).Scan(&lendAmount, &splitOf); err != nil {
		return err
	}
	total := lendAmount
	if splitOf.Valid {
		var billAmount float64
		if err := tx.QueryRowContext(ctx, `SELECT amount FROM fin_transactions WHERE id = $1 AND user_id = $2 FOR UPDATE`, splitOf.Int64, uid).Scan(&billAmount); err != nil {
			return err
		}
		total += billAmount
	}
	total = roundMoney(total)
	if share <= 0 || share > total {
		return lendErr("Their share has to be above zero and at most the bill's ₹%.2f.", total)
	}
	if share < roundMoney(direct) {
		return lendErr("Their share can't be below the ₹%.2f already repaid.", roundMoney(direct))
	}

	switch {
	case share == total && splitOf.Valid:
		// Whole again: the bill row becomes the lend, the split row goes.
		bill := splitOf.Int64
		if _, err := tx.ExecContext(ctx, `UPDATE fin_lends SET source_transaction_id = $1, principal = $2, updated_at = NOW() WHERE id = $3`, bill, total, lendID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2`, lendTxn, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'lend', amount = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, total, bill, uid); err != nil {
			return err
		}
	case share == total:
		// Already whole.
	case !splitOf.Valid:
		// Whole → split: the original row keeps its id (and any capture ref)
		// as the user's expense; the share is cut into a new row.
		var cut int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions
			(user_id, account_id, category_id, type, amount, description, txn_at, slate_id, split_of)
			SELECT user_id, account_id, category_id, 'lend', $1, description, txn_at, slate_id, id
			FROM fin_transactions WHERE id = $2 AND user_id = $3 RETURNING id`, share, lendTxn, uid).Scan(&cut); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_lends SET source_transaction_id = $1, principal = $2, updated_at = NOW() WHERE id = $3`, cut, share, lendID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'expense', amount = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, total-share, lendTxn, uid); err != nil {
			return err
		}
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET amount = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, share, lendTxn, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET amount = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, total-share, splitOf.Int64, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_lends SET principal = $1, updated_at = NOW() WHERE id = $2`, share, lendID); err != nil {
			return err
		}
	}
	return RebalanceLends(ctx, tx, uid, borrower)
}

// Forgive writes off up to what borrower still owes. The amount becomes a
// 'writeoff' row: spending on the day it was forgiven (in categoryID), with
// no money moving, since the lend already moved it. It is pooled with the
// person's settlements, so it clears their oldest lends first, and undoing
// it (Unsettle) deletes it.
func Forgive(ctx context.Context, tx *sql.Tx, uid, borrower string, amount float64, categoryID *int64, at time.Time, note string) (int64, error) {
	borrower = strings.TrimSpace(borrower)
	amount = roundMoney(amount)
	if borrower == "" || amount <= 0 {
		return 0, lendErr("Pick a person and an amount above zero.")
	}
	var owed float64
	var accountID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(l.principal),0)
		- COALESCE((SELECT SUM(r.amount) FROM fin_lend_repayments r JOIN fin_lends rl ON rl.id = r.lend_id
			WHERE rl.user_id = $1 AND LOWER(BTRIM(rl.borrower)) = LOWER(BTRIM($2))
			AND NOT EXISTS (SELECT 1 FROM fin_lend_settlements s WHERE s.transaction_id = r.transaction_id)),0)
		- COALESCE((SELECT SUM(t.amount) FROM fin_lend_settlements s JOIN fin_transactions t ON t.id = s.transaction_id
			WHERE s.user_id = $1 AND LOWER(BTRIM(s.borrower)) = LOWER(BTRIM($2))),0),
		(SELECT source_account_id FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+` ORDER BY lent_at DESC, id DESC LIMIT 1)
		FROM fin_lends l WHERE l.user_id = $1 AND LOWER(BTRIM(l.borrower)) = LOWER(BTRIM($2))`, uid, borrower).Scan(&owed, &accountID); err != nil {
		return 0, err
	}
	owed = roundMoney(owed)
	if !accountID.Valid || owed <= 0 {
		return 0, lendErr("%s doesn't owe anything right now.", borrower)
	}
	if amount > owed {
		return 0, lendErr("%s owes ₹%.2f, so you can forgive up to that.", borrower, owed)
	}
	var txnID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, note, txn_at)
		VALUES ($1,$2,$3,'writeoff',$4,$5,$6,$7) RETURNING id`,
		uid, accountID.Int64, categoryID, amount, "Forgave "+borrower, strings.TrimSpace(note), at,
	).Scan(&txnID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fin_lend_settlements (user_id, borrower, transaction_id) VALUES ($1,$2,$3)`, uid, borrower, txnID); err != nil {
		return 0, err
	}
	return txnID, RebalanceLends(ctx, tx, uid, borrower)
}

// SettleWith converts credits into settlements from borrower.
func SettleWith(ctx context.Context, tx *sql.Tx, uid, borrower string, txnIDs []int64) error {
	borrower = strings.TrimSpace(borrower)
	if borrower == "" || len(txnIDs) == 0 {
		return lendErr("Pick a person and at least one transaction.")
	}
	// Names match case-insensitively; keep the spelling already on their lends.
	var known string
	tx.QueryRowContext(ctx, `SELECT borrower FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+` ORDER BY lent_at DESC LIMIT 1`, uid, borrower).Scan(&known)
	if known == "" {
		return lendErr("%s doesn't owe anything right now.", borrower)
	}
	borrower = known
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
	// A forgiven amount was never money received: undoing it removes it.
	if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2 AND type = 'writeoff'`, txnID, uid); err != nil {
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
	// A split folds its share back into the bill it was cut from.
	var splitOf sql.NullInt64
	var share float64
	if err := tx.QueryRowContext(ctx, `SELECT split_of, amount FROM fin_transactions WHERE id = $1 AND user_id = $2`, txnID, uid).Scan(&splitOf, &share); err != nil {
		return err
	}
	if splitOf.Valid {
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2`, txnID, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET amount = amount + $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, share, splitOf.Int64, uid); err != nil {
			return err
		}
		return RebalanceLends(ctx, tx, uid, borrower)
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

// Execer is the slice of *sql.Tx / *sql.DB that SyncSplit needs.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SyncSplit keeps a split bill's share on the bill's account, date and slate
// after the bill is edited, so the pair still reads as one charge.
func SyncSplit(ctx context.Context, q Execer, uid string, billID int64) error {
	if _, err := q.ExecContext(ctx, `UPDATE fin_transactions s SET account_id = b.account_id, txn_at = b.txn_at,
		slate_id = b.slate_id, description = b.description, updated_at = NOW()
		FROM fin_transactions b WHERE s.split_of = b.id AND b.id = $1 AND b.user_id = $2 AND s.user_id = $2`, billID, uid); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `UPDATE fin_lends l SET source_account_id = s.account_id, lent_at = s.txn_at, updated_at = NOW()
		FROM fin_transactions s WHERE l.source_transaction_id = s.id AND s.split_of = $1 AND s.user_id = $2 AND l.user_id = $2`, billID, uid)
	return err
}
