package db

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"
)

// Money the user owes people (fin_owed): the mirror of lends, kept simple.
// A debt is a 'borrow' (money in, not income) or, when no money moved, an
// 'owed' row; a payment is a 'borrow_repayment' (money out, not spending) or
// the fin_owed half of a net-off. What the user owes a person is their debts
// minus their payments; nothing is matched debt by debt.

const personMatch = "LOWER(BTRIM(person)) = LOWER(BTRIM($2))"

// OwedTo is what the user owes person now (debts minus payments; never
// below zero).
func OwedTo(ctx context.Context, q Execer, uid, person string) (float64, error) {
	var v float64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN o.kind = 'debt' THEN t.amount ELSE -t.amount END), 0)
		FROM fin_owed o JOIN fin_transactions t ON t.id = o.transaction_id
		WHERE o.user_id = $1 AND LOWER(BTRIM(o.person)) = LOWER(BTRIM($2))`, uid, person).Scan(&v)
	return math.Max(roundMoney(v), 0), err
}

// owedRaw is debts minus payments without the floor, to catch payments that
// would exceed the debts.
func owedRaw(ctx context.Context, q Execer, uid, person string) (float64, error) {
	var v float64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN o.kind = 'debt' THEN t.amount ELSE -t.amount END), 0)
		FROM fin_owed o JOIN fin_transactions t ON t.id = o.transaction_id
		WHERE o.user_id = $1 AND LOWER(BTRIM(o.person)) = LOWER(BTRIM($2))`, uid, person).Scan(&v)
	return roundMoney(v), err
}

// CheckOwed refuses a state where the user has paid person back more than
// they owed. Run it after any change to a person's debts or payments.
func CheckOwed(ctx context.Context, q Execer, uid, person string) error {
	v, err := owedRaw(ctx, q, uid, person)
	if err != nil {
		return err
	}
	if v < -0.004 {
		return lendErr("That pays %s back ₹%.2f more than you owe them.", strings.TrimSpace(person), -v)
	}
	return nil
}

// knownLender is the spelling already used for person on a debt, or "".
func knownLender(ctx context.Context, q Execer, uid, person string) string {
	var name string
	q.QueryRowContext(ctx, `SELECT o.person FROM fin_owed o JOIN fin_transactions t ON t.id = o.transaction_id
		WHERE o.user_id = $1 AND `+strings.Replace(personMatch, "person", "o.person", 1)+` ORDER BY t.txn_at DESC LIMIT 1`, uid, person).Scan(&name)
	return name
}

func validDue(due *string) (any, error) {
	if due == nil || *due == "" {
		return nil, nil
	}
	if _, err := time.Parse("2006-01-02", *due); err != nil {
		return nil, lendErr("Pick a valid date to pay it back by.")
	}
	return *due, nil
}

// Borrow marks existing income as money the user owes person back. It stays
// on its account (the balance doesn't change) but is no longer income.
func Borrow(ctx context.Context, tx *sql.Tx, uid, person string, txnID int64, due *string) error {
	person = strings.TrimSpace(person)
	if person == "" {
		return lendErr("Pick who you owe it to.")
	}
	dueV, err := validDue(due)
	if err != nil {
		return err
	}
	if k := knownLender(ctx, tx, uid, person); k != "" {
		person = k
	}
	var typ string
	var pair sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT type, transfer_pair FROM fin_transactions WHERE id = $1 AND user_id = $2 FOR UPDATE`, txnID, uid).Scan(&typ, &pair)
	if errors.Is(err, sql.ErrNoRows) {
		return lendErr("That transaction no longer exists.")
	}
	if err != nil {
		return err
	}
	if typ != "income" || pair.Valid {
		return lendErr("Only plain income can be money you owe back.")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'borrow', updated_at = NOW() WHERE id = $1 AND user_id = $2`, txnID, uid); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO fin_owed (user_id, person, transaction_id, kind, due_date) VALUES ($1,$2,$3,'debt',$4)`, uid, person, txnID, dueV)
	return err
}

// PayBack marks an existing expense as paying person back: money out that
// isn't spending.
func PayBack(ctx context.Context, tx *sql.Tx, uid, person string, txnID int64) error {
	person = strings.TrimSpace(person)
	known := knownLender(ctx, tx, uid, person)
	if known == "" {
		return lendErr("You don't owe %s anything.", person)
	}
	var typ string
	var pair sql.NullInt64
	var shared bool
	err := tx.QueryRowContext(ctx, `SELECT type, transfer_pair,
		EXISTS (SELECT 1 FROM fin_transactions sp WHERE sp.split_of = t.id)
		OR EXISTS (SELECT 1 FROM fin_investment_contributions c WHERE c.txn_id = t.id)
		FROM fin_transactions t WHERE id = $1 AND user_id = $2 FOR UPDATE`, txnID, uid).Scan(&typ, &pair, &shared)
	if errors.Is(err, sql.ErrNoRows) {
		return lendErr("That transaction no longer exists.")
	}
	if err != nil {
		return err
	}
	if typ != "expense" || pair.Valid || shared {
		return lendErr("Only a plain expense can pay someone back.")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = 'borrow_repayment', updated_at = NOW() WHERE id = $1 AND user_id = $2`, txnID, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fin_owed (user_id, person, transaction_id, kind) VALUES ($1,$2,$3,'payment')`, uid, known, txnID); err != nil {
		return err
	}
	return CheckOwed(ctx, tx, uid, known)
}

// OwedDebt is a new debt recorded in one go (not from an existing row).
type OwedDebt struct {
	Person      string
	AccountID   int64
	Amount      float64
	Description string
	Note        string
	At          time.Time
	SlateID     int64
	Due         *string
	// NoMoneyMoved: owed from before Sajni. The row moves no money.
	NoMoneyMoved bool
}

// AddDebt records money the user owes: a 'borrow' into AccountID, or an
// 'owed' row when no money moved. Returns the transaction id.
func AddDebt(ctx context.Context, tx *sql.Tx, uid string, d OwedDebt) (int64, error) {
	d.Person = strings.TrimSpace(d.Person)
	d.Amount = roundMoney(d.Amount)
	d.Description = strings.TrimSpace(d.Description)
	if d.Person == "" || d.Amount <= 0 {
		return 0, lendErr("Enter who you owe and an amount above zero.")
	}
	if d.Description == "" {
		d.Description = "From " + d.Person
	}
	dueV, err := validDue(d.Due)
	if err != nil {
		return 0, err
	}
	if k := knownLender(ctx, tx, uid, d.Person); k != "" {
		d.Person = k
	}
	typ := "borrow"
	if d.NoMoneyMoved {
		typ = "owed"
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, note, txn_at, slate_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, uid, d.AccountID, typ, d.Amount, d.Description, strings.TrimSpace(d.Note), d.At, d.SlateID).Scan(&id); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO fin_owed (user_id, person, transaction_id, kind, due_date) VALUES ($1,$2,$3,'debt',$4)`, uid, d.Person, id, dueV)
	return id, err
}

// SetOwedDue changes when the user means to pay a debt back ("" clears it).
func SetOwedDue(ctx context.Context, tx *sql.Tx, uid string, owedID int64, due string) error {
	dueV, err := validDue(&due)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE fin_owed SET due_date = $1 WHERE id = $2 AND user_id = $3 AND kind = 'debt'`, dueV, owedID, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return lendErr("That debt no longer exists.")
	}
	return nil
}

// Unowe undoes one fin_owed row: a borrow goes back to income, a payment
// back to an expense; an owed row (no money moved) is deleted, and a net-off
// is undone on both sides.
func Unowe(ctx context.Context, tx *sql.Tx, uid string, owedID int64) error {
	var person, typ string
	var txnID int64
	err := tx.QueryRowContext(ctx, `SELECT o.person, o.transaction_id, t.type FROM fin_owed o JOIN fin_transactions t ON t.id = o.transaction_id
		WHERE o.id = $1 AND o.user_id = $2 FOR UPDATE OF o`, owedID, uid).Scan(&person, &txnID, &typ)
	if errors.Is(err, sql.ErrNoRows) {
		return lendErr("That no longer exists.")
	}
	if err != nil {
		return err
	}
	switch typ {
	case "netoff":
		return undoNetOff(ctx, tx, uid, txnID)
	case "owed":
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2`, txnID, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE user_id = $1 AND entity_type = 'transaction' AND entity_id = $2`, uid, txnID); err != nil {
			return err
		}
	default:
		back := "income"
		if typ == "borrow_repayment" {
			back = "expense"
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_owed WHERE id = $1 AND user_id = $2`, owedID, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, back, txnID, uid); err != nil {
			return err
		}
	}
	if err := CheckOwed(ctx, tx, uid, person); err != nil {
		return lendErr("You've already paid %s back more than that would leave. Undo a payment first.", person)
	}
	return nil
}

// lendTotal is what person owes the user: lends minus every settlement.
func lendTotal(ctx context.Context, q Execer, uid, person string) (float64, sql.NullInt64, error) {
	var owed float64
	var accountID sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT SUM(principal) FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+`),0)
		- COALESCE((SELECT SUM(t.amount) FROM fin_lend_settlements s JOIN fin_transactions t ON t.id = s.transaction_id
			WHERE s.user_id = $1 AND LOWER(BTRIM(s.borrower)) = LOWER(BTRIM($2))),0),
		(SELECT source_account_id FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+` ORDER BY lent_at DESC, id DESC LIMIT 1)`,
		uid, person).Scan(&owed, &accountID)
	return roundMoney(owed), accountID, err
}

// NetOff closes the smaller of what person owes the user and what the user
// owes person against each other. No money moves: a pair of 'netoff' rows,
// one settling the lends (overall), one paying the debt. Returns the amount.
func NetOff(ctx context.Context, tx *sql.Tx, uid, person string, at time.Time) (float64, error) {
	person = strings.TrimSpace(person)
	theyOwe, accountID, err := lendTotal(ctx, tx, uid, person)
	if err != nil {
		return 0, err
	}
	iOwe, err := OwedTo(ctx, tx, uid, person)
	if err != nil {
		return 0, err
	}
	amount := roundMoney(math.Min(theyOwe, iOwe))
	if amount <= 0 || !accountID.Valid {
		return 0, lendErr("There's nothing to net off: one of you owes the other nothing.")
	}
	var lendName string
	tx.QueryRowContext(ctx, `SELECT borrower FROM fin_lends WHERE user_id = $1 AND `+borrowerMatch+` ORDER BY lent_at DESC LIMIT 1`, uid, person).Scan(&lendName)
	owedName := knownLender(ctx, tx, uid, person)
	title := "Netted with " + lendName
	var a, b int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, slate_id)
		VALUES ($1,$2,'netoff',$3,$4,$5,(SELECT id FROM fin_slates WHERE user_id = $1 AND is_plain)) RETURNING id`,
		uid, accountID.Int64, amount, title, at).Scan(&a); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, slate_id, transfer_pair)
		VALUES ($1,$2,'netoff',$3,$4,$5,(SELECT id FROM fin_slates WHERE user_id = $1 AND is_plain),$6) RETURNING id`,
		uid, accountID.Int64, amount, title, at, a).Scan(&b); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET transfer_pair = $1 WHERE id = $2`, b, a); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fin_lend_settlements (user_id, borrower, transaction_id) VALUES ($1,$2,$3)`, uid, lendName, a); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fin_owed (user_id, person, transaction_id, kind) VALUES ($1,$2,$3,'payment')`, uid, owedName, b); err != nil {
		return 0, err
	}
	return amount, RebalanceLends(ctx, tx, uid, lendName)
}

// undoNetOff removes both halves of a net-off, given either one.
func undoNetOff(ctx context.Context, tx *sql.Tx, uid string, txnID int64) error {
	var pair sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT transfer_pair FROM fin_transactions WHERE id = $1 AND user_id = $2 AND type = 'netoff'`, txnID, uid).Scan(&pair); err != nil {
		return err
	}
	ids := []int64{txnID}
	if pair.Valid {
		ids = append(ids, pair.Int64)
	}
	var borrower string
	for _, id := range ids {
		var b string
		if tx.QueryRowContext(ctx, `DELETE FROM fin_lend_settlements WHERE transaction_id = $1 AND user_id = $2 RETURNING borrower`, id, uid).Scan(&b) == nil {
			borrower = b
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_lend_repayments WHERE transaction_id = $1 AND user_id = $2`, id, uid); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET transfer_pair = NULL WHERE id = $1 AND user_id = $2`, id, uid); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2`, id, uid); err != nil {
			return err
		}
	}
	if borrower != "" {
		return RebalanceLends(ctx, tx, uid, borrower)
	}
	return nil
}

// AddLendBefore records that borrower owed the user money from before Sajni:
// a lend whose row ('owed') moves no money. Returns the lend id.
func AddLendBefore(ctx context.Context, tx *sql.Tx, uid, borrower string, accountID int64, amount float64, description, note string, at time.Time, slateID int64) (int64, error) {
	borrower = strings.TrimSpace(borrower)
	amount = roundMoney(amount)
	description = strings.TrimSpace(description)
	if borrower == "" || amount <= 0 {
		return 0, lendErr("Enter who owes you and an amount above zero.")
	}
	if description == "" {
		description = "Owed before Sajni"
	}
	var txnID, lendID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, note, txn_at, slate_id)
		VALUES ($1,$2,'owed',$3,$4,$5,$6,$7) RETURNING id`, uid, accountID, amount, description, strings.TrimSpace(note), at, slateID).Scan(&txnID); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_lends (user_id, source_account_id, source_transaction_id, borrower, principal, description, note, lent_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, uid, accountID, txnID, borrower, amount, description, strings.TrimSpace(note), at).Scan(&lendID); err != nil {
		return 0, err
	}
	return lendID, RebalanceLends(ctx, tx, uid, borrower)
}
