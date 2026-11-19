package ai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"sajni/internal/db"
)

// Recurring items (fin_billers): the logic lives in db/recurring.go, shared
// with the API and its cron; these tools only read arguments.

func listBillersTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	includeArchived := argBool(args, "include_archived", false)
	q := `SELECT b.id, b.name, b.kind, b.amount, b.frequency, b.next_due_date::text,
		a.name, b.auto_renew, b.alert_days, b.income,
		(SELECT MAX(paid_date)::text FROM fin_biller_payments p WHERE p.biller_id = b.id),
		COALESCE((SELECT MAX(due_date)::text FROM fin_biller_payments p WHERE p.biller_id = b.id), ''),
		(SELECT id FROM fin_biller_payments p WHERE p.biller_id = b.id ORDER BY due_date DESC LIMIT 1)
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
	now := time.Now().In(userTZLoc(ctx, d, uid))
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, kind, freq, due, lastDue string
		var amount float64
		var account, lastPaid sql.NullString
		var lastPayment sql.NullInt64
		var autoRenew, income bool
		var alertDays int
		if err := rows.Scan(&id, &name, &kind, &amount, &freq, &due, &account, &autoRenew, &alertDays, &income, &lastPaid, &lastDue, &lastPayment); err != nil {
			return nil, nil, err
		}
		row := map[string]any{
			"id": id, "name": name, "kind": kind, "amount": amount,
			"frequency": freq, "next_due_date": due,
			"auto_renew": autoRenew, "alert_days": alertDays,
		}
		if income {
			row["income"] = true
		}
		if account.Valid {
			row["account_name"] = account.String
		}
		if lastPaid.Valid {
			row["last_paid_date"] = lastPaid.String
		}
		if lastPayment.Valid {
			row["last_payment_id"] = lastPayment.Int64
		}
		next, _ := time.Parse("2006-01-02", due[:10])
		row["state"], _ = db.RecurringState(next, lastDue, lastPaid.String, now)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	res := map[string]any{"items": out, "count": len(out)}
	if sugg, err := db.RecurringSuggestions(ctx, d, uid); err == nil && len(sugg) > 0 {
		res["suggestions"] = sugg
	}
	return res, nil, nil
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
	if kind == "subscription" && amount <= 0 {
		return nil, nil, fmt.Errorf("a fixed amount is required (or kind 'bill' when it varies)")
	}
	freq := argStr(args, "frequency")
	if freq == "" {
		freq = "monthly"
	}
	if !db.ValidRecurringFrequency(freq) {
		return nil, nil, fmt.Errorf("invalid frequency %q", freq)
	}
	due := argStr(args, "next_due_date")
	if due == "" {
		due = time.Now().In(userTZLoc(ctx, d, uid)).Format("2006-01-02")
	}
	dueT, err := time.Parse("2006-01-02", due)
	if err != nil {
		return nil, nil, fmt.Errorf("next_due_date must be YYYY-MM-DD")
	}
	accountID := argInt(args, "account_id", 0)
	autoRenew := argBool(args, "auto_renew", false) && kind == "subscription" // an amount that varies can't be posted ahead
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
	err = d.QueryRowContext(ctx, `INSERT INTO fin_billers
		(user_id, name, kind, amount, frequency, next_due_date, anchor_day, account_id, category_id,
		 auto_renew, remind_task, alert_days, notes, income)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		uid, name, kind, amount, freq, due, dueT.Day(), accountArg, categoryArg,
		autoRenew, argBool(args, "remind_task", false), alertDays, argStr(args, "notes"), argBool(args, "income", false)).Scan(&id)
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
	p := db.RecurringPay{
		DueDate:      argStr(args, "due_date"),
		PaidDate:     argStr(args, "paid_date"),
		AttachTxnIDs: argInt64Slice(args, "attach_txn_ids"),
	}
	if v := argFloat(args, "amount"); v > 0 {
		p.Amount = &v
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	paid, err := db.PayRecurring(ctx, tx, uid, id, p)
	if errors.Is(err, db.ErrRecurringNotFound) {
		return nil, nil, fmt.Errorf("recurring item not found")
	}
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "txn_id": paid.TxnID, "already_paid": paid.AlreadyPaid, "next_due_date": paid.NextDue},
		map[string]any{"kind": "biller_paid", "id": id, "title": paid.Name, "route": "/finance/billers"}, nil
}

func unlinkBillerPaymentTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id, pid := argInt(args, "biller_id", 0), argInt(args, "payment_id", 0)
	if id == 0 || pid == 0 {
		return nil, nil, fmt.Errorf("biller_id and payment_id are required")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	next, err := db.UnlinkRecurringPayment(ctx, tx, uid, id, pid)
	if errors.Is(err, db.ErrRecurringNotFound) {
		return nil, nil, fmt.Errorf("recurring item not found")
	}
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "unlinked": pid, "next_due_date": next},
		map[string]any{"kind": "biller_paid", "id": id, "title": "Cycle undone", "route": "/finance/billers"}, nil
}

func dismissRecurringSuggestionTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	key := argStr(args, "key")
	if err := db.DismissRecurringSuggestion(ctx, d, uid, key); err != nil {
		return nil, nil, err
	}
	return map[string]any{"dismissed": key}, map[string]any{"kind": "biller_paid", "id": 0, "title": "Suggestion hidden", "route": "/finance/billers"}, nil
}
