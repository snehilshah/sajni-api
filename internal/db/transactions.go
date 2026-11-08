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
// of the invested amount. Lending rows are refused: they change from Lends.
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
		return &TxnConflict{"This is a lending entry. Change it from Lends."}
	case "writeoff":
		return &TxnConflict{"This is an adjusted amount. Undo it from Lends."}
	}
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
	for _, del := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2`, del, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE user_id = $1 AND entity_type = 'transaction' AND entity_id = $2`, uid, del); err != nil {
			return err
		}
	}
	if toInvestment {
		return FollowInvestmentTransfer(ctx, tx, uid, id, invID, invOld)
	}
	return nil
}
