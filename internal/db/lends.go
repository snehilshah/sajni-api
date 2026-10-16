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
// person; "forgive" books a writeoff that settles like a credit. Every
// settlement reduces the person's total. One pinned to a lend (lend_id) also
// pays that lend off (Rebalance derives fin_lend_repayments from the pins);
// one left "overall" is never attributed to any lend. See fin_lend_state.

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
	err := tx.QueryRowContext(ctx, `SELECT origin, borrower, source_transaction_id
		FROM fin_lends WHERE id = $1 AND user_id = $2 FOR UPDATE`, lendID, uid).Scan(&origin, &borrower, &lendTxn)
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
// no money moving, since the lend already moved it. It settles like a
// payment (toward lendID, or overall when nil), and undoing it (Unsettle)
// deletes it.
func Forgive(ctx context.Context, tx *sql.Tx, uid, borrower string, amount float64, categoryID *int64, at time.Time, note string, lendID *int64) (int64, error) {
	borrower = strings.TrimSpace(borrower)
	amount = roundMoney(amount)
	if borrower == "" || amount <= 0 {
		return 0, lendErr("Pick a person and an amount above zero.")
	}
	var owed float64
	var accountID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(l.principal),0)
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
		return 0, lendErr("%s owes ₹%.2f, so you can adjust up to that.", borrower, owed)
	}
	if err := checkPin(ctx, tx, uid, borrower, lendID); err != nil {
		return 0, err
	}
	var txnID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, note, txn_at)
		VALUES ($1,$2,$3,'writeoff',$4,$5,$6,$7) RETURNING id`,
		uid, accountID.Int64, categoryID, amount, "Adjusted with "+borrower, strings.TrimSpace(note), at,
	).Scan(&txnID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fin_lend_settlements (user_id, borrower, transaction_id, lend_id) VALUES ($1,$2,$3,$4)`, uid, borrower, txnID, lendID); err != nil {
		return 0, err
	}
	return txnID, RebalanceLends(ctx, tx, uid, borrower)
}

// EditAdjustment changes an adjustment's (a writeoff's) amount, category or
// time; nil leaves one as it is. No money moved, so all three are free to
// change: the amount stays within what the person owes without it, and the
// rebalance re-checks that it isn't dated before what it settles.
func EditAdjustment(ctx context.Context, tx *sql.Tx, uid string, txnID int64, amount *float64, categoryID *int64, at *time.Time) error {
	var borrower string
	if err := tx.QueryRowContext(ctx, `SELECT s.borrower FROM fin_lend_settlements s JOIN fin_transactions t ON t.id = s.transaction_id
		WHERE t.id = $1 AND t.user_id = $2 AND t.type = 'writeoff'`, txnID, uid).Scan(&borrower); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lendErr("That adjustment no longer exists.")
		}
		return err
	}
	if amount != nil {
		a := roundMoney(*amount)
		if a <= 0 {
			return lendErr("Enter an amount above zero.")
		}
		var room float64
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT SUM(principal) FROM fin_lends WHERE user_id = $1 AND LOWER(BTRIM(borrower)) = LOWER(BTRIM($2))),0)
			- COALESCE((SELECT SUM(t.amount) FROM fin_lend_settlements s JOIN fin_transactions t ON t.id = s.transaction_id
				WHERE s.user_id = $1 AND LOWER(BTRIM(s.borrower)) = LOWER(BTRIM($2)) AND t.id <> $3),0)`, uid, borrower, txnID).Scan(&room); err != nil {
			return err
		}
		if room = roundMoney(room); a > room {
			return lendErr("%s owes ₹%.2f without this, so you can adjust up to that.", borrower, room)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET amount = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, a, txnID, uid); err != nil {
			return err
		}
	}
	if categoryID != nil {
		res, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET category_id = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3
			AND EXISTS (SELECT 1 FROM fin_categories WHERE id = $1 AND user_id = $3 AND kind = 'expense')`, *categoryID, txnID, uid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return lendErr("Pick one of your expense categories.")
		}
	}
	if at != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET txn_at = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, *at, txnID, uid); err != nil {
			return err
		}
	}
	return RebalanceLends(ctx, tx, uid, borrower)
}

// checkPin makes sure a pinned lend is one of borrower's. nil = overall.
func checkPin(ctx context.Context, tx *sql.Tx, uid, borrower string, lendID *int64) error {
	if lendID == nil {
		return nil
	}
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_lends WHERE id = $3 AND user_id = $1 AND `+borrowerMatch+`)`,
		uid, borrower, *lendID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return lendErr("That lend isn't one of %s's.", borrower)
	}
	return nil
}

// SettleWith converts credits into settlements from borrower, toward lendID
// (nil = their overall total).
func SettleWith(ctx context.Context, tx *sql.Tx, uid, borrower string, txnIDs []int64, lendID *int64) error {
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
	if err := checkPin(ctx, tx, uid, borrower, lendID); err != nil {
		return err
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO fin_lend_settlements (user_id, borrower, transaction_id, lend_id) VALUES ($1,$2,$3,$4)`, uid, borrower, txnID, lendID); err != nil {
			return err
		}
	}
	return RebalanceLends(ctx, tx, uid, borrower)
}

// SetSettlementLend re-points a payment (or forgive) toward one of the
// person's lends, or back to their overall total (nil).
func SetSettlementLend(ctx context.Context, tx *sql.Tx, uid string, settlementID int64, lendID *int64) error {
	var borrower string
	err := tx.QueryRowContext(ctx, `SELECT borrower FROM fin_lend_settlements WHERE id = $1 AND user_id = $2 FOR UPDATE`, settlementID, uid).Scan(&borrower)
	if errors.Is(err, sql.ErrNoRows) {
		return lendErr("That payment no longer exists.")
	}
	if err != nil {
		return err
	}
	if err := checkPin(ctx, tx, uid, borrower, lendID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_lend_settlements SET lend_id = $1 WHERE id = $2 AND user_id = $3`, lendID, settlementID, uid); err != nil {
		return err
	}
	return RebalanceLends(ctx, tx, uid, borrower)
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
// Payments pinned to it fall back to the person's overall total.
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

// RebalanceLends re-derives which of a person's lends their pinned payments
// cover, oldest payment first, each up to what is left of its lend. Anything
// beyond that, and every unpinned payment, only lowers the person's total
// (fin_lend_state). Deterministic, so it runs after every change.
func RebalanceLends(ctx context.Context, tx *sql.Tx, uid, borrower string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, principal FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+` ORDER BY id FOR UPDATE`, uid, borrower)
	if err != nil {
		return err
	}
	room := map[int64]float64{}
	for rows.Next() {
		var id int64
		var principal float64
		if err := rows.Scan(&id, &principal); err != nil {
			rows.Close()
			return err
		}
		room[id] = principal
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fin_lend_repayments r USING fin_lend_settlements s
		WHERE r.user_id = $1 AND s.user_id = $1 AND r.transaction_id = s.transaction_id AND LOWER(BTRIM(s.borrower)) = LOWER(BTRIM($2))`, uid, borrower); err != nil {
		return err
	}

	pins, err := tx.QueryContext(ctx, `SELECT s.lend_id, t.id, t.account_id, t.amount, t.txn_at FROM fin_lend_settlements s
		JOIN fin_transactions t ON t.id = s.transaction_id
		WHERE s.user_id = $1 AND LOWER(BTRIM(s.borrower)) = LOWER(BTRIM($2)) AND s.lend_id IS NOT NULL
		ORDER BY t.txn_at, t.id`, uid, borrower)
	if err != nil {
		return err
	}
	type pin struct {
		lendID, txnID, accountID int64
		amount                   float64
		at                       time.Time
	}
	var list []pin
	for pins.Next() {
		var p pin
		if err := pins.Scan(&p.lendID, &p.txnID, &p.accountID, &p.amount, &p.at); err != nil {
			pins.Close()
			return err
		}
		list = append(list, p)
	}
	pins.Close()
	if err := pins.Err(); err != nil {
		return err
	}
	for _, p := range list {
		left, ok := room[p.lendID] // a pin to someone else's lend counts overall
		if !ok || left <= 0.004 {
			continue
		}
		part := roundMoney(math.Min(p.amount, left))
		if _, err := tx.ExecContext(ctx, `INSERT INTO fin_lend_repayments
			(user_id, lend_id, destination_account_id, transaction_id, amount, repaid_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			uid, p.lendID, p.accountID, p.txnID, part, p.at); err != nil {
			return err
		}
		room[p.lendID] = left - part
	}
	return CheckLendOrder(ctx, tx, uid, borrower)
}

// Execer is the slice of *sql.Tx / *sql.DB that SyncSplit needs.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CheckLendOrder refuses a payment back (or a forgive) dated before what it
// pays: the lend it is toward or, when it counts overall, the person's first
// lend. It compares the user's calendar days, so a same-day payment entered
// with an earlier time still passes. It runs after every lend change
// (Rebalance, SyncSplit), so a date edit on either side is caught too.
func CheckLendOrder(ctx context.Context, q Execer, uid, borrower string) error {
	var kind, who, paidOn, owedOn string
	var pinned bool
	err := q.QueryRowContext(ctx, `
		WITH tz AS (SELECT COALESCE(NULLIF(timezone,''),'Asia/Kolkata') AS z FROM users WHERE id = $1),
		lends AS (SELECT id, (lent_at AT TIME ZONE (SELECT z FROM tz))::date AS day FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+`)
		SELECT t.type, s.borrower, to_char((t.txn_at AT TIME ZONE (SELECT z FROM tz))::date, 'FMDD Mon'),
		       to_char(COALESCE(p.day, (SELECT MIN(day) FROM lends)), 'FMDD Mon'), p.id IS NOT NULL
		FROM fin_lend_settlements s
		JOIN fin_transactions t ON t.id = s.transaction_id
		LEFT JOIN lends p ON p.id = s.lend_id
		WHERE s.user_id = $1 AND LOWER(BTRIM(s.borrower)) = LOWER(BTRIM($2))
		  AND (t.txn_at AT TIME ZONE (SELECT z FROM tz))::date < COALESCE(p.day, (SELECT MIN(day) FROM lends))
		ORDER BY t.txn_at LIMIT 1`, uid, borrower).Scan(&kind, &who, &paidOn, &owedOn, &pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	what := "payment from " + who
	if kind == "writeoff" {
		what = "adjustment for " + who
	}
	if pinned {
		return lendErr("The %s on %s would come before the lend it's toward (%s). Check the dates.", what, paidOn, owedOn)
	}
	return lendErr("The %s on %s would come before anything they owed (first lend %s). Check the dates.", what, paidOn, owedOn)
}

// SyncSplit keeps a split bill's share on the bill's account, date and slate
// after the bill is edited, so the pair still reads as one charge.
func SyncSplit(ctx context.Context, q Execer, uid string, billID int64) error {
	if _, err := q.ExecContext(ctx, `UPDATE fin_transactions s SET account_id = b.account_id, txn_at = b.txn_at,
		slate_id = b.slate_id, description = b.description, updated_at = NOW()
		FROM fin_transactions b WHERE s.split_of = b.id AND b.id = $1 AND b.user_id = $2 AND s.user_id = $2`, billID, uid); err != nil {
		return err
	}
	var borrower string
	err := q.QueryRowContext(ctx, `UPDATE fin_lends l SET source_account_id = s.account_id, lent_at = s.txn_at, updated_at = NOW()
		FROM fin_transactions s WHERE l.source_transaction_id = s.id AND s.split_of = $1 AND s.user_id = $2 AND l.user_id = $2
		RETURNING l.borrower`, billID, uid).Scan(&borrower)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The share's date moved with the bill: its payments must still follow it.
	return CheckLendOrder(ctx, q, uid, borrower)
}
