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
