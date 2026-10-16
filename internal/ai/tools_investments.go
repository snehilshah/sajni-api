package ai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"sajni/internal/db"
)

// setInvestmentAutoDebitTool mirrors the validation in api.updateInvestment.
func setInvestmentAutoDebitTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "investment_id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing investment_id")
	}
	enabled := argBool(args, "enabled", false)

	var name, freq string
	var monthly float64
	var acct *int64
	var start *string
	if err := d.QueryRowContext(ctx, `SELECT name, account_id, monthly_amount, frequency, start_date::text
		FROM fin_investments WHERE id = $1 AND user_id = $2`, id, uid).Scan(&name, &acct, &monthly, &freq, &start); err != nil {
		return nil, nil, fmt.Errorf("investment not found")
	}
	if a := argInt(args, "account_id", 0); a > 0 {
		acct = &a
		d.ExecContext(ctx, `UPDATE fin_investments SET account_id = $1 WHERE id = $2 AND user_id = $3`, a, id, uid)
	}

	if !enabled {
		d.ExecContext(ctx, `UPDATE fin_investments SET auto_debit = FALSE, next_debit_date = NULL, last_updated = NOW()
			WHERE id = $1 AND user_id = $2`, id, uid)
		return map[string]any{"id": id, "auto_debit": false},
			map[string]any{"kind": "investment_updated", "id": id, "title": name, "route": "/finance/investments"}, nil
	}

	if acct == nil {
		return nil, nil, fmt.Errorf("auto-debit needs a linked account")
	}
	if monthly <= 0 {
		return nil, nil, fmt.Errorf("auto-debit needs a per-cycle amount on the investment")
	}
	switch freq {
	case "monthly", "quarterly", "yearly":
	default:
		return nil, nil, fmt.Errorf("auto-debit needs a recurring frequency (monthly/quarterly/yearly), got %q", freq)
	}
	next := argStr(args, "next_debit_date")
	if next == "" {
		// Project start_date's day-of-month to the next occurrence >= today.
		now := userTZNow(ctx, d, uid)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		next = today.Format("2006-01-02")
		if start != nil && *start != "" {
			if s, err := time.Parse("2006-01-02", *start); err == nil {
				if !s.Before(today) {
					next = s.Format("2006-01-02")
				} else {
					anchor := s.Day()
					cand := anchoredMonthDate(today.Year(), today.Month(), anchor)
					if cand.Before(today) {
						nextMonth := time.Date(today.Year(), today.Month()+1, 1, 0, 0, 0, 0, time.UTC)
						cand = anchoredMonthDate(nextMonth.Year(), nextMonth.Month(), anchor)
					}
					next = cand.Format("2006-01-02")
				}
			}
		}
	}
	nextDate, err := time.Parse("2006-01-02", next)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid next_debit_date")
	}
	if _, err := d.ExecContext(ctx, `UPDATE fin_investments SET auto_debit = TRUE, next_debit_date = $1, anchor_day = $2, last_updated = NOW()
		WHERE id = $3 AND user_id = $4`, next, nextDate.Day(), id, uid); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "auto_debit": true, "next_debit_date": next},
		map[string]any{"kind": "investment_updated", "id": id, "title": name, "route": "/finance/investments"}, nil
}

func anchoredMonthDate(year int, month time.Month, day int) time.Time {
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > last {
		day = last
	}
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// listInvestmentsTool lists the user's investments for picking one (a
// transfer into it, auto-debit).
func listInvestmentsTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	rows, err := d.QueryContext(ctx, `SELECT i.id, i.name, i.type, i.invested_amount, i.monthly_amount, i.frequency,
		i.account_id, COALESCE(a.name, ''), i.auto_debit, COALESCE(i.next_debit_date::text, ''), COALESCE(i.maturity_date::text, '')
		FROM fin_investments i LEFT JOIN fin_accounts a ON a.id = i.account_id
		WHERE i.user_id = $1 ORDER BY i.name`, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, typ, freq, acctName, next, maturity string
		var invested, cycle float64
		var acct sql.NullInt64
		var auto bool
		if err := rows.Scan(&id, &name, &typ, &invested, &cycle, &freq, &acct, &acctName, &auto, &next, &maturity); err != nil {
			return nil, nil, err
		}
		row := map[string]any{"id": id, "name": name, "type": typ, "invested_amount": invested,
			"per_cycle": cycle, "frequency": freq, "auto_debit": auto}
		if acct.Valid {
			row["account_id"], row["account_name"] = acct.Int64, acctName
		}
		if next != "" {
			row["next_debit_date"] = next
		}
		if maturity != "" {
			row["maturity_date"] = maturity
		}
		out = append(out, row)
	}
	return out, nil, rows.Err()
}

// transferToInvestmentTool is create_transaction's type 'transfer' with
// to_investment_id: money from an account into an investment.
func transferToInvestmentTool(ctx context.Context, d *db.DB, uid string, acct int64, amount float64, desc string, txnAt time.Time, args map[string]any) (any, map[string]any, error) {
	invID := argInt(args, "to_investment_id", 0)
	if invID == 0 {
		return nil, nil, fmt.Errorf("a transfer here needs to_investment_id (from list_investments)")
	}
	var owned bool
	d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_accounts WHERE id = $1 AND user_id = $2)`, acct, uid).Scan(&owned)
	if !owned {
		return nil, nil, fmt.Errorf("account not found")
	}
	slate := argInt(args, "slate_id", 0)
	if slate > 0 {
		var okSlate bool
		d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_slates WHERE id = $1 AND user_id = $2 AND NOT archived)`, slate, uid).Scan(&okSlate)
		if !okSlate {
			return nil, nil, fmt.Errorf("slate not found")
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	id, err := db.TransferToInvestment(ctx, tx, uid, acct, invID, amount, strings.TrimSpace(desc), strings.TrimSpace(argStr(args, "note")), txnAt, slate)
	if errors.Is(err, db.ErrInvestmentNotFound) {
		return nil, nil, fmt.Errorf("investment not found")
	}
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "amount": amount, "type": "transfer_out", "investment_id": invID},
		map[string]any{"kind": "transaction_created", "id": id, "title": fmt.Sprintf("transfer %.2f", amount), "route": "/finance/transactions"}, nil
}
