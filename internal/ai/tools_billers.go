package ai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"sajni/internal/db"
)

func listBillersTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	includeArchived := argBool(args, "include_archived", false)
	q := `SELECT b.id, b.name, b.kind, b.amount, b.frequency, b.next_due_date::text,
		a.name, b.auto_renew, b.alert_days
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
		var name, kind, freq, due string
		var amount float64
		var account sql.NullString
		var autoRenew bool
		var alertDays int
		rows.Scan(&id, &name, &kind, &amount, &freq, &due, &account, &autoRenew, &alertDays)
		row := map[string]any{
			"id": id, "name": name, "kind": kind, "amount": amount,
			"frequency": freq, "next_due_date": due,
			"auto_renew": autoRenew, "alert_days": alertDays,
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
	kind := argStr(args, "kind")
	if kind == "" {
		kind = "subscription"
	}
	if kind != "subscription" && kind != "bill" {
		return nil, nil, fmt.Errorf("invalid kind %q (subscription or bill)", kind)
	}
	amount := argFloat(args, "amount")
	// Subscriptions have a fixed price; bills carry an optional estimate.
	if kind == "subscription" && amount <= 0 {
		return nil, nil, fmt.Errorf("a subscription needs a fixed amount")
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
	autoRenew := argBool(args, "auto_renew", false) && kind == "subscription" // bills never auto-pay
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
		(user_id, name, kind, amount, frequency, next_due_date, account_id, category_id,
		 auto_renew, remind_task, variable, alert_days, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		uid, name, kind, amount, freq, due, accountArg, categoryArg,
		autoRenew, argBool(args, "remind_task", false), kind == "bill", alertDays, argStr(args, "notes")).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "name": name, "kind": kind, "next_due_date": due},
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
	if v := argFloat(args, "amount"); v > 0 {
		amount = v
	}
	paid := argStr(args, "paid_date")
	if paid == "" {
		paid = time.Now().Format("2006-01-02")
	}
	attachIDs := argInt64Slice(args, "attach_txn_ids")

	// Mirrors api.postBillerTxn / attachBillerTxns (duplicated to avoid an
	// import cycle between internal/ai and internal/api): payment row FIRST
	// inside one tx so UNIQUE(biller_id, due_date) gates the money movement.
	var txnID int64
	alreadyPaid := false
	if len(attachIDs) > 0 {
		// Attach mode: link existing txns, create nothing.
		var cnt int
		var sum float64
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(amount),0)
			FROM fin_transactions WHERE user_id = $1 AND id = ANY($2)`, uid, attachIDs).Scan(&cnt, &sum); err != nil {
			return nil, nil, err
		}
		if cnt != len(attachIDs) {
			return nil, nil, fmt.Errorf("one or more transactions not found")
		}
		if argFloat(args, "amount") <= 0 {
			amount = sum
		}
		tx, err := d.Begin()
		if err != nil {
			return nil, nil, err
		}
		defer tx.Rollback()
		var paymentID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO fin_biller_payments (user_id, biller_id, due_date, paid_date, amount, auto)
			VALUES ($1,$2,$3,$4,$5,FALSE) ON CONFLICT (biller_id, due_date) DO NOTHING RETURNING id`,
			uid, id, due, paid, amount).Scan(&paymentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				alreadyPaid = true
			} else {
				return nil, nil, err
			}
		} else {
			for _, tid := range attachIDs {
				tx.ExecContext(ctx, `INSERT INTO fin_biller_payment_txns (payment_id, txn_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
					paymentID, tid)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, err
		}
	} else {
		if !accountID.Valid {
			return nil, nil, fmt.Errorf("biller has no account assigned")
		}
		var catArg any
		if categoryID.Valid {
			catArg = categoryID.Int64
		}
		tx, err := d.Begin()
		if err != nil {
			return nil, nil, err
		}
		defer tx.Rollback()
		var paymentID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO fin_biller_payments (user_id, biller_id, due_date, paid_date, amount, auto)
			VALUES ($1,$2,$3,$4,$5,FALSE) ON CONFLICT (biller_id, due_date) DO NOTHING RETURNING id`,
			uid, id, due, paid, amount).Scan(&paymentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				if cerr := tx.Commit(); cerr != nil {
					return nil, nil, cerr
				}
				return map[string]any{"id": id, "already_paid": true}, nil, nil
			}
			return nil, nil, err
		}
		// Biller pay is a system path — no pocket (General).
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at)
			 VALUES ($1,$2,$3,'expense',$4,$5,($6::timestamp AT TIME ZONE 'Asia/Kolkata')) RETURNING id`,
			uid, accountID.Int64, catArg, amount, name, paid).Scan(&txnID); err != nil {
			return nil, nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_biller_payments SET txn_id = $1 WHERE id = $2`, txnID, paymentID); err != nil {
			return nil, nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO fin_biller_payment_txns (payment_id, txn_id) VALUES ($1,$2)`, paymentID, txnID); err != nil {
			return nil, nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, err
		}
	}
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
	return map[string]any{"id": id, "txn_id": txnID, "already_paid": alreadyPaid, "next_due_date": next.Format("2006-01-02")},
		map[string]any{"kind": "biller_paid", "id": id, "title": name, "route": "/finance/billers"}, nil
}
