package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sajni/internal/db"
)

// Pocket tools mirror internal/api/pockets.go (duplicated queries to avoid
// an import cycle between internal/ai and internal/api).

func listPocketsTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	now := userTZNow(ctx, d, uid)
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	from := first.Format("2006-01-02")
	to := first.AddDate(0, 1, -1).Format("2006-01-02")

	rows, err := d.QueryContext(ctx, `SELECT id, name, is_active FROM fin_pockets
		WHERE user_id = $1 AND archived = FALSE ORDER BY created_at ASC`, uid)
	if err != nil {
		return nil, nil, err
	}
	items := []map[string]any{}
	var activeID any
	for rows.Next() {
		var id int64
		var name string
		var active bool
		rows.Scan(&id, &name, &active)
		if active {
			activeID = id
		}
		items = append(items, map[string]any{"id": id, "name": name, "is_active": active})
	}
	rows.Close()

	var generalSpend float64
	srows, serr := d.QueryContext(ctx, `SELECT pocket_id, COALESCE(SUM(amount),0)
		FROM fin_transactions WHERE user_id = $1 AND type = 'expense'
		AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3
		GROUP BY pocket_id`, uid, from, to)
	if serr == nil {
		spend := map[int64]float64{}
		for srows.Next() {
			var pid *int64
			var sum float64
			srows.Scan(&pid, &sum)
			if pid == nil {
				generalSpend = sum
			} else {
				spend[*pid] = sum
			}
		}
		srows.Close()
		for _, it := range items {
			it["month_spend"] = spend[it["id"].(int64)]
		}
	}
	return map[string]any{
		"items": items, "count": len(items),
		"general_spend": generalSpend, "active_pocket_id": activeID,
	}, nil, nil
}

func createPocketTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	name := strings.TrimSpace(argStr(args, "name"))
	if name == "" {
		return nil, nil, fmt.Errorf("missing name")
	}
	var dup int
	d.QueryRowContext(ctx, `SELECT 1 FROM fin_pockets WHERE user_id = $1 AND LOWER(name) = LOWER($2) AND NOT archived`,
		uid, name).Scan(&dup)
	if dup == 1 {
		return nil, nil, fmt.Errorf("a pocket named %q already exists", name)
	}
	color := argStr(args, "color")
	if color == "" {
		color = "#2D5A4F"
	}
	var id int64
	if err := d.QueryRowContext(ctx, `INSERT INTO fin_pockets (user_id, name, color) VALUES ($1,$2,$3) RETURNING id`,
		uid, name, color).Scan(&id); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "name": name},
		map[string]any{"kind": "pocket_created", "id": id, "title": name, "route": "/finance"}, nil
}

func setActivePocketTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	pid := argInt(args, "pocket_id", 0)
	tx, err := d.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE fin_pockets SET is_active = FALSE WHERE user_id = $1 AND is_active`, uid); err != nil {
		return nil, nil, err
	}
	name := ""
	if pid > 0 {
		res, err := tx.ExecContext(ctx, `UPDATE fin_pockets SET is_active = TRUE WHERE id = $1 AND user_id = $2 AND NOT archived`, pid, uid)
		if err != nil {
			return nil, nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, nil, fmt.Errorf("pocket not found")
		}
		tx.QueryRowContext(ctx, `SELECT name FROM fin_pockets WHERE id = $1`, pid).Scan(&name)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	if pid > 0 {
		return map[string]any{"active_pocket_id": pid, "name": name},
			map[string]any{"kind": "pocket_activated", "id": pid, "title": name, "route": "/finance"}, nil
	}
	return map[string]any{"active_pocket_id": nil},
		map[string]any{"kind": "pocket_activated", "id": int64(0), "title": "General", "route": "/finance"}, nil
}

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
