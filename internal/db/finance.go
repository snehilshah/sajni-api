package db

import (
	"context"
	"fmt"
	"strings"
)

// SpendTypes are the transaction types that move spending: expenses and
// forgiven lends add to it, refunds take away. Filter with
// `type IN `+SpendTypes and sum Spend(alias), never raw amount.
const SpendTypes = "('expense','writeoff','refund')"

// Spend is the signed spending of a row of fin_transactions aliased alias
// ("" for an unaliased table). Pair it with the SpendTypes filter.
func Spend(alias string) string {
	if alias != "" {
		alias += "."
	}
	return fmt.Sprintf("(CASE WHEN %[1]stype = 'refund' THEN -%[1]samount ELSE %[1]samount END)", alias)
}

// MoneyInTypes and MoneyOutTypes move an account's balance up and down.
// Writeoffs, owed rows and net-offs move no money, so they're in neither.
const (
	MoneyInTypes  = "('income','refund','transfer_in','lend_repayment','borrow')"
	MoneyOutTypes = "('expense','transfer_out','lend','borrow_repayment')"
)

// BalanceDelta is a fin_transactions row's signed effect on its account's
// balance (alias "" for an unaliased table).
func BalanceDelta(alias string) string {
	if alias != "" {
		alias += "."
	}
	return fmt.Sprintf("(CASE WHEN %[1]stype IN %[2]s THEN %[1]samount WHEN %[1]stype IN %[3]s THEN -%[1]samount ELSE 0 END)",
		alias, MoneyInTypes, MoneyOutTypes)
}

// IgnoreCaptureAccount is "Don't track": messages whose account hint is hint
// (lowercased) are ignored from now on, and the reviews already waiting for
// that account are settled as ignored. Returns those capture ids, so the
// phone can clear their notifications.
func IgnoreCaptureAccount(ctx context.Context, d *DB, uid, hint, label string) ([]int64, error) {
	hint = strings.ToLower(strings.TrimSpace(hint))
	if label = strings.TrimSpace(label); label == "" {
		label = hint
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO fin_capture_ignores (user_id, hint, label) VALUES ($1,$2,$3)
		ON CONFLICT (user_id, hint) DO UPDATE SET label = EXCLUDED.label`, uid, hint, label); err != nil {
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `UPDATE fin_captures SET outcome = 'ignored', reason = 'Not tracked: ' || $3
		WHERE user_id = $1 AND outcome = 'review' AND LOWER(BTRIM(account_hint)) = $2 RETURNING id`, uid, hint, label)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
