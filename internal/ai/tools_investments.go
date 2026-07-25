package ai

import (
	"context"
	"fmt"
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
