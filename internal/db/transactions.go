package db

import (
	"context"
	"database/sql"
	"errors"
)

// ErrTxnNotFound is returned when the transaction isn't the user's.
var ErrTxnNotFound = errors.New("transaction not found")

// TxnConflict is a delete refused because the row belongs to something else
// (a lend, an adjustment, a split). Msg is shown to the user as is.
type TxnConflict struct{ Msg string }

func (e *TxnConflict) Error() string { return e.Msg }

// DeleteTransaction deletes one of the user's transactions with its tags and,
// for a transfer, its other leg. A payment into an investment comes back out
// of the invested amount. Lending rows are refused: they change from People.
// The API and the AI tools both go through here.
func DeleteTransaction(ctx context.Context, tx *sql.Tx, uid string, id int64) error {
	var storedType string
	var pair sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT type, transfer_pair FROM fin_transactions WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		id, uid).Scan(&storedType, &pair)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTxnNotFound
	}
	if err != nil {
		return err
	}
	switch storedType {
	case "lend", "lend_repayment":
		return &TxnConflict{"This is a lending entry. Change it from People."}
	case "writeoff":
		return &TxnConflict{"This is an adjusted amount. Undo it from People."}
	case "netoff":
		return &TxnConflict{"This was netted off. Undo it from People."}
	case "owed":
		var lend bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_lends WHERE source_transaction_id = $1)`, id).Scan(&lend); err != nil {
			return err
		}
		if lend {
			return &TxnConflict{"This is a lending entry. Change it from People."}
		}
	}
	// Money the user owes someone: deleting a debt can't leave them paid back
	// more than they owed.
	var owedTo string
	tx.QueryRowContext(ctx, `SELECT person FROM fin_owed WHERE transaction_id = $1 AND user_id = $2`, id, uid).Scan(&owedTo)
	var splitWith string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(string_agg(l.borrower, ', ' ORDER BY sp.id), '') FROM fin_transactions sp
		JOIN fin_lends l ON l.source_transaction_id = sp.id
		WHERE sp.split_of = $1 AND sp.user_id = $2`, id, uid).Scan(&splitWith); err != nil {
		return err
	}
	if splitWith != "" {
		return &TxnConflict{"This bill is split with " + splitWith + ". Remove the split first."}
	}
	invID, invOld, toInvestment, err := InvestmentTransfer(ctx, tx, uid, id)
	if err != nil {
		return err
	}
	ids := []int64{id}
	if pair.Valid {
		ids = append(ids, pair.Int64)
	}
	// Money back from an investment takes its gain (the Interest row filed
	// with it) along.
	if toInvestment && storedType == "transfer_in" {
		rows, err := tx.QueryContext(ctx, `SELECT g.id FROM fin_transactions g JOIN fin_transactions t ON t.id = $1
			WHERE g.user_id = $2 AND g.type = 'income' AND g.investment_id = t.investment_id
			  AND g.account_id = t.account_id AND g.txn_at = t.txn_at`, id, uid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var g int64
			if rows.Scan(&g) == nil {
				ids = append(ids, g)
			}
		}
		rows.Close()
	}
	for _, del := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2`, del, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE user_id = $1 AND entity_type = 'transaction' AND entity_id = $2`, uid, del); err != nil {
			return err
		}
	}
	if owedTo != "" {
		if err := CheckOwed(ctx, tx, uid, owedTo); err != nil {
			return &TxnConflict{"You've already paid " + owedTo + " back more than that would leave. Delete a payment first."}
		}
	}
	if toInvestment {
		return FollowInvestmentTransfer(ctx, tx, uid, id, invID, invOld)
	}
	return nil
}

// MakeTransfer turns one of the user's plain expenses or incomes into a
// transfer with another of their accounts: money that only moved between
// their own accounts (an Amazon Pay top-up, a UPI Lite load, cash a friend
// handed back by UPI). An expense becomes the transfer's "out" leg and the
// other account gets the "in" leg; an income becomes the "in" leg and the
// other account the "out" one. Title, note, time and slate carry over; the
// category goes (a transfer has none). Rows tied to something else (a
// split, a lend, a refund, a biller, an investment, money owed) are
// refused. Returns the new leg's id. The API, the capture endpoint and the
// AI tools all go through here.
func MakeTransfer(ctx context.Context, tx *sql.Tx, uid string, id, otherAccountID int64) (int64, error) {
	var (
		typ                 string
		acct, slate         sql.NullInt64
		amount              float64
		desc, note          string
		txnAt               any
		splitOf, investment sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `SELECT type, account_id, amount, description, note, txn_at, slate_id, split_of, investment_id
		FROM fin_transactions WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, uid).
		Scan(&typ, &acct, &amount, &desc, &note, &txnAt, &slate, &splitOf, &investment)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrTxnNotFound
	}
	if err != nil {
		return 0, err
	}
	if typ != "expense" && typ != "income" {
		return 0, &TxnConflict{"Only an expense or an income can become a transfer."}
	}
	if splitOf.Valid || investment.Valid {
		return 0, &TxnConflict{"This row belongs to a split or an investment, so it can't become a transfer."}
	}
	if !acct.Valid || acct.Int64 == otherAccountID {
		return 0, &TxnConflict{"Pick a different account for the other side."}
	}
	var ok bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_accounts WHERE id = $1 AND user_id = $2)`, otherAccountID, uid).Scan(&ok); err != nil {
		return 0, err
	}
	if !ok {
		return 0, &TxnConflict{"That account doesn't exist."}
	}
	var tied bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM fin_transactions WHERE (split_of = $1 OR refund_of = $1) AND user_id = $2)
		OR EXISTS (SELECT 1 FROM fin_lends WHERE source_transaction_id = $1)
		OR EXISTS (SELECT 1 FROM fin_lend_repayments WHERE transaction_id = $1)
		OR EXISTS (SELECT 1 FROM fin_lend_settlements WHERE transaction_id = $1)
		OR EXISTS (SELECT 1 FROM fin_owed WHERE transaction_id = $1)
		OR EXISTS (SELECT 1 FROM fin_biller_payment_txns WHERE txn_id = $1)`, id, uid).Scan(&tied); err != nil {
		return 0, err
	}
	if tied {
		return 0, &TxnConflict{"This row is tied to a split, refund, lend, bill or money owed, so it can't become a transfer."}
	}
	thisLeg, otherLeg := "transfer_out", "transfer_in"
	if typ == "income" {
		thisLeg, otherLeg = "transfer_in", "transfer_out"
	}
	var newID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions
		(user_id, account_id, type, amount, description, note, txn_at, slate_id, transfer_pair, linked_account)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		uid, otherAccountID, otherLeg, amount, desc, note, txnAt, slate, id, acct.Int64).Scan(&newID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET type = $1, category_id = NULL, refund_of = NULL,
		transfer_pair = $2, linked_account = $3, updated_at = NOW() WHERE id = $4 AND user_id = $5`,
		thisLeg, newID, otherAccountID, id, uid); err != nil {
		return 0, err
	}
	if err := SyncTags(ctx, tx, uid, "transaction", newID, note); err != nil {
		return 0, err
	}
	return newID, nil
}

// LearnPayeeTransfer remembers that the owner turned a captured row into a
// transfer with otherAccount (MakeTransfer), keyed on the payee the capture
// read. Changing the account starts the count again. The API and AI tools
// call it; the capture endpoint's own pairing doesn't (an "Amazon Pay"
// top-up pair mustn't turn every Amazon Pay purchase into a transfer).
func LearnPayeeTransfer(ctx context.Context, tx *sql.Tx, uid string, txnID, otherAccount int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO fin_payee_transfers (user_id, payee, kind, account_id, own, last_txn_id)
		SELECT c.user_id, LOWER(BTRIM(c.description)), c.type, $3, c.own_money, $1
		FROM fin_captures c
		WHERE c.txn_id = $1 AND c.user_id = $2 AND c.outcome = 'created' AND c.description <> '' AND c.type IN ('expense','income')
		ORDER BY c.id LIMIT 1
		ON CONFLICT (user_id, payee, kind) DO UPDATE SET
			uses = CASE WHEN fin_payee_transfers.account_id <> EXCLUDED.account_id THEN 1
			            WHEN fin_payee_transfers.last_txn_id = EXCLUDED.last_txn_id THEN fin_payee_transfers.uses
			            ELSE fin_payee_transfers.uses + 1 END,
			own = EXCLUDED.own OR (fin_payee_transfers.own AND fin_payee_transfers.account_id = EXCLUDED.account_id),
			account_id = EXCLUDED.account_id, last_txn_id = EXCLUDED.last_txn_id, updated_at = NOW()`,
		txnID, uid, otherAccount); err != nil {
		return err
	}
	return nil
}
