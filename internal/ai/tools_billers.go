package ai

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"sajni/internal/db"
)

func listBillersTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	includeArchived := argBool(args, "include_archived", false)
	q := `SELECT b.id, b.name, b.amount, b.frequency, b.next_due_date::text,
		a.name, b.is_subscription, b.auto_renew, b.alert_days
		FROM fin_billers b LEFT JOIN fin_accounts a ON a.id = b.account_id
		WHERE b.user_id = $1`
	if !includeArchived {
		q += " AND b.archived = FALSE"
	}
	q += " ORDER BY b.next_due_date ASC LIMIT 100"
	rows, err := d.QueryContext(ctx, q, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, freq, due string
		var amount float64
		var account sql.NullString
		var isSub, autoRenew bool
		var alertDays int
		rows.Scan(&id, &name, &amount, &freq, &due, &account, &isSub, &autoRenew, &alertDays)
		row := map[string]any{
			"id": id, "name": name, "amount": amount,
			"frequency": freq, "next_due_date": due,
			"is_subscription": isSub, "auto_renew": autoRenew, "alert_days": alertDays,
		}
		if account.Valid {
			row["account_name"] = account.String
		}
		out = append(out, row)
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func createBillerTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	name := argStr(args, "name")
	if name == "" {
		return nil, nil, fmt.Errorf("missing name")
	}
	amount := argFloat(args, "amount")
	if amount <= 0 {
		return nil, nil, fmt.Errorf("amount must be positive")
	}
	freq := argStr(args, "frequency")
	if freq == "" {
		freq = "monthly"
	}
	switch freq {
	case "weekly", "fortnightly", "monthly", "bimonthly":
	default:
		return nil, nil, fmt.Errorf("invalid frequency %q", freq)
	}
	due := argStr(args, "next_due_date")
	if due == "" {
		due = time.Now().Format("2006-01-02")
	}
	accountID := argInt(args, "account_id", 0)
	autoRenew := argBool(args, "auto_renew", false)
	if autoRenew && accountID == 0 {
		return nil, nil, fmt.Errorf("auto_renew requires account_id")
	}
	categoryID := argInt(args, "category_id", 0)
	alertDays := int(argInt(args, "alert_days", 3))

	var accountArg, categoryArg any
	if accountID > 0 {
		accountArg = accountID
	}
	if categoryID > 0 {
		categoryArg = categoryID
	}

	var id int64
	err := d.QueryRowContext(ctx, `INSERT INTO fin_billers
		(user_id, name, amount, frequency, next_due_date, account_id, category_id,
		 is_subscription, auto_renew, remind_task, alert_days, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		uid, name, amount, freq, due, accountArg, categoryArg,
		argBool(args, "is_subscription", false), autoRenew, argBool(args, "remind_task", false), alertDays, argStr(args, "notes")).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "name": name, "next_due_date": due},
		map[string]any{"kind": "biller_created", "id": id, "title": name, "route": "/finance/billers"}, nil
}

func payBillerTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "biller_id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing biller_id")
	}
	var name, freq, due string
	var amount float64
	var accountID, categoryID sql.NullInt64
	err := d.QueryRowContext(ctx, `SELECT name, amount, frequency, next_due_date::text, account_id, category_id
		FROM fin_billers WHERE id = $1 AND user_id = $2`, id, uid).
		Scan(&name, &amount, &freq, &due, &accountID, &categoryID)
	if err != nil {
		return nil, nil, fmt.Errorf("biller not found")
	}
	if !accountID.Valid {
		return nil, nil, fmt.Errorf("biller has no account assigned")
	}
	if v := argFloat(args, "amount"); v > 0 {
		amount = v
	}
	paid := argStr(args, "paid_date")
	if paid == "" {
		paid = time.Now().Format("2006-01-02")
	}
	// Mirror api.postBillerTxn (kept duplicated here to avoid an import cycle
	// between internal/ai and internal/api). The same UNIQUE constraint on
	// (biller_id, due_date) keeps this idempotent.
	var exists int
	d.QueryRowContext(ctx, `SELECT 1 FROM fin_biller_payments WHERE biller_id=$1 AND due_date=$2`,
		id, due).Scan(&exists)
	if exists == 1 {
		return map[string]any{"id": id, "already_paid": true}, nil, nil
	}
	var catArg any
	if categoryID.Valid {
		catArg = categoryID.Int64
	}
	var txnID int64
	err = d.QueryRowContext(ctx,
		`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_date)
		 VALUES ($1,$2,$3,'expense',$4,$5,$6) RETURNING id`,
		uid, accountID.Int64, catArg, amount, name, paid).Scan(&txnID)
	if err != nil {
		return nil, nil, err
	}
	d.ExecContext(ctx,
		`INSERT INTO fin_biller_payments (user_id, biller_id, txn_id, due_date, paid_date, amount, auto)
		 VALUES ($1,$2,$3,$4,$5,$6,FALSE) ON CONFLICT (biller_id, due_date) DO NOTHING`,
		uid, id, txnID, due, paid, amount)
	dueT, _ := time.Parse("2006-01-02", due)
	next := dueT
	switch freq {
	case "weekly":
		next = next.AddDate(0, 0, 7)
	case "fortnightly":
		next = next.AddDate(0, 0, 14)
	case "bimonthly":
		next = next.AddDate(0, 2, 0)
	default:
		next = next.AddDate(0, 1, 0)
	}
	d.ExecContext(ctx, `UPDATE fin_billers SET next_due_date=$1, updated_at=NOW(), last_run_at=NOW() WHERE id=$2`,
		next.Format("2006-01-02"), id)
	return map[string]any{"id": id, "txn_id": txnID, "next_due_date": next.Format("2006-01-02")},
		map[string]any{"kind": "biller_paid", "id": id, "title": name, "route": "/finance/billers"}, nil
}
