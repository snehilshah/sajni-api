package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Recurring items (table fin_billers, shown as Billers until the rename):
// Home Contrib ₹22,000 on the 5th, Salary expected around the 5th, ChatGPT.
// An item waits for its payment: one that fits (written by any path:
// capture, the sheet, the AI) attaches itself to the cycle (MatchRecurring).
// "Post it by itself" (auto_renew) is a per-item choice. kind 'subscription'
// = a fixed amount, 'bill' = it varies (the amount is an estimate); income
// marks money expected in (Salary). The API, its cron and the AI tools all
// pay through PayRecurring.

// A payment fits a cycle from RecurringBefore days before its due date to
// RecurringAfter days after (Home Contrib due 5 Nov, paid 6 Nov 01:26).
const (
	RecurringBefore = 3
	RecurringAfter  = 7
)

// RecurringFrequencies are the repeat choices, in the order clients list them.
var RecurringFrequencies = []string{"weekly", "fortnightly", "monthly", "bimonthly", "quarterly", "yearly"}

// ValidRecurringFrequency reports whether f is one of RecurringFrequencies.
func ValidRecurringFrequency(f string) bool {
	for _, x := range RecurringFrequencies {
		if x == f {
			return true
		}
	}
	return false
}

// AdvanceDue moves a due date one period forward. Months keep the anchor
// day where the month has it (a 31st is the 30th in November and the 31st
// again in December), so the day never drifts.
func AdvanceDue(d time.Time, freq string, anchorDay int) time.Time {
	switch freq {
	case "weekly":
		return d.AddDate(0, 0, 7)
	case "fortnightly":
		return d.AddDate(0, 0, 14)
	case "bimonthly":
		return addMonthsAnchored(d, 2, anchorDay)
	case "quarterly":
		return addMonthsAnchored(d, 3, anchorDay)
	case "yearly":
		return addMonthsAnchored(d, 12, anchorDay)
	default:
		return addMonthsAnchored(d, 1, anchorDay)
	}
}

func addMonthsAnchored(d time.Time, months, anchorDay int) time.Time {
	if anchorDay < 1 || anchorDay > 31 {
		anchorDay = d.Day()
	}
	first := time.Date(d.Year(), d.Month()+time.Month(months), 1, 0, 0, 0, 0, d.Location())
	last := first.AddDate(0, 1, -1).Day()
	if anchorDay > last {
		anchorDay = last
	}
	return time.Date(first.Year(), first.Month(), anchorDay, 0, 0, 0, 0, d.Location())
}

// RecurringCycleOn returns the cycle a day belongs to, starting from the
// item's next due date: the latest cycle whose window has opened by day.
// Before next's window it is next itself.
func RecurringCycleOn(next time.Time, freq string, anchorDay int, day time.Time) time.Time {
	c := next
	for i := 0; i < 400; i++ {
		n := AdvanceDue(c, freq, anchorDay)
		if n.AddDate(0, 0, -RecurringBefore).After(day) {
			return c
		}
		c = n
	}
	return c
}

// RecurringPay says which cycle to record and how.
type RecurringPay struct {
	DueDate      string   // the cycle the caller saw ("" = the item's current one)
	PaidDate     string   // the user's calendar day ("" = today)
	Amount       *float64 // overrides the item's amount (a bill's actual)
	AttachTxnIDs []int64  // link these rows instead of posting one
	Auto         bool     // posted by the cron (auto_renew)
}

// RecurringPaid is what PayRecurring did.
type RecurringPaid struct {
	Name        string
	TxnID       int64 // the posted row (0 when attaching or already paid)
	AlreadyPaid bool
	NextDue     string
}

// ErrRecurringNotFound: no such item for this user.
var ErrRecurringNotFound = errors.New("recurring item not found")

// PayRecurring records one cycle of a recurring item as paid, posting a row
// on its account or linking existing ones, and moves the due date past it.
// A cycle is recorded once: a second tap that saw the same due date (or an
// older client's tap within seconds of the first) reads "already paid"
// instead of paying the next cycle too.
func PayRecurring(ctx context.Context, tx *sql.Tx, uid string, id int64, p RecurringPay) (RecurringPaid, error) {
	var out RecurringPaid
	var amount float64
	var freq, next, tz string
	var anchor int
	var income bool
	var account, category sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT b.name, b.amount, b.frequency, b.next_due_date::text,
			COALESCE(b.anchor_day, EXTRACT(DAY FROM b.next_due_date)::INTEGER), b.income, b.account_id, b.category_id,
			COALESCE(NULLIF(u.timezone,''),'Asia/Kolkata')
		FROM fin_billers b JOIN users u ON u.id = b.user_id
		WHERE b.id = $1 AND b.user_id = $2 FOR UPDATE OF b`, id, uid).
		Scan(&out.Name, &amount, &freq, &next, &anchor, &income, &account, &category, &tz)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrRecurringNotFound
	}
	if err != nil {
		return out, err
	}
	loc, lerr := time.LoadLocation(tz)
	if lerr != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	now := time.Now().In(loc)
	if p.PaidDate == "" {
		p.PaidDate = now.Format("2006-01-02")
	}
	if p.Amount != nil {
		amount = *p.Amount
	}
	due := p.DueDate
	if due == "" {
		due = next
		// An older client sends no due date: a second tap seconds after the
		// first is the same tap, not next month's payment.
		var recent bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_biller_payments
			WHERE biller_id = $1 AND created_at > NOW() - INTERVAL '30 seconds')`, id).Scan(&recent); err != nil {
			return out, err
		}
		if recent {
			out.AlreadyPaid, out.NextDue = true, next
			return out, nil
		}
	}
	dueT, err := time.Parse("2006-01-02", due)
	if err != nil {
		return out, fmt.Errorf("invalid due date %q", due)
	}
	nextT, _ := time.Parse("2006-01-02", next)

	var paymentID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO fin_biller_payments (user_id, biller_id, due_date, paid_date, amount, auto)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (biller_id, due_date) DO NOTHING RETURNING id`,
		uid, id, due, p.PaidDate, amount, p.Auto).Scan(&paymentID)
	if errors.Is(err, sql.ErrNoRows) {
		out.AlreadyPaid, out.NextDue = true, next
		return out, nil
	}
	if err != nil {
		return out, err
	}

	if len(p.AttachTxnIDs) > 0 {
		var n int
		var sum float64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND id = ANY($2)`,
			uid, p.AttachTxnIDs).Scan(&n, &sum); err != nil {
			return out, err
		}
		if n != len(p.AttachTxnIDs) {
			return out, errors.New("one or more transactions not found")
		}
		if p.Amount == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE fin_biller_payments SET amount = $1 WHERE id = $2`, sum, paymentID); err != nil {
				return out, err
			}
		}
		for _, t := range p.AttachTxnIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fin_biller_payment_txns (payment_id, txn_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, paymentID, t); err != nil {
				return out, err
			}
		}
	} else {
		if !account.Valid {
			return out, errors.New("this item has no account: pick one before paying")
		}
		// Today's payment is now; another day's (a back-dated tick, the
		// cron) is noon, so it never lands on the day before.
		at := now
		if p.PaidDate != now.Format("2006-01-02") {
			day, err := time.ParseInLocation("2006-01-02", p.PaidDate, loc)
			if err != nil {
				return out, fmt.Errorf("invalid paid date %q", p.PaidDate)
			}
			at = day.Add(12 * time.Hour)
		}
		typ := "expense"
		if income {
			typ = "income"
		}
		var cat any
		if category.Valid {
			cat = category.Int64
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, uid, account.Int64, cat, typ, amount, out.Name, at).Scan(&out.TxnID); err != nil {
			return out, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO fin_biller_payment_txns (payment_id, txn_id) VALUES ($1,$2)`, paymentID, out.TxnID); err != nil {
			return out, err
		}
	}

	// The current cycle (or a later one, skipping missed ones) moves the
	// due date; an older cycle paid late leaves it.
	out.NextDue = next
	if !dueT.Before(nextT) {
		out.NextDue = AdvanceDue(dueT, freq, anchor).Format("2006-01-02")
		if _, err := tx.ExecContext(ctx, `UPDATE fin_billers SET next_due_date = $1, updated_at = NOW(), last_run_at = NOW() WHERE id = $2`,
			out.NextDue, id); err != nil {
			return out, err
		}
	}
	return out, nil
}

// RecurringMatch is the item a payment was attached to.
type RecurringMatch struct {
	ID    int64
	Name  string
	Due   string // the cycle it paid
	Title string // the row's title afterwards
}

// MatchRecurring attaches a just-written expense or income to the recurring
// item it fits, best effort: the same direction, the item's account (when it
// has one), the amount (a fixed one within 2% or ₹1; one that varies within
// half to double its estimate, or any when it has none), and a day inside a
// cycle's window that isn't paid yet. The closest amount wins. A row capture
// just wrote (captured) takes the item's name and category; a typed row in
// Unknown takes only its category. A cycle Sajni posted by itself gives
// way to the real payment. Returns ok=false when nothing fits.
func MatchRecurring(ctx context.Context, tx *sql.Tx, uid string, txnID int64, captured bool) (RecurringMatch, bool, error) {
	var m RecurringMatch
	var typ, day string
	var amount float64
	var account sql.NullInt64
	var unknown bool
	err := tx.QueryRowContext(ctx, `SELECT t.type, t.amount, t.account_id,
			(t.txn_at AT TIME ZONE COALESCE(NULLIF(u.timezone,''),'Asia/Kolkata'))::date::text,
			t.category_id IS NULL OR EXISTS (SELECT 1 FROM fin_categories k WHERE k.id = t.category_id AND k.fallback)
		FROM fin_transactions t JOIN users u ON u.id = t.user_id
		WHERE t.id = $1 AND t.user_id = $2`, txnID, uid).Scan(&typ, &amount, &account, &day, &unknown)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if typ != "expense" && typ != "income" {
		return m, false, nil
	}
	var linked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_biller_payment_txns WHERE txn_id = $1)`, txnID).Scan(&linked); err != nil || linked {
		return m, false, err
	}
	dayT, _ := time.Parse("2006-01-02", day)

	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, amount, frequency, next_due_date::text,
			COALESCE(anchor_day, EXTRACT(DAY FROM next_due_date)::INTEGER), auto_renew
		FROM fin_billers
		WHERE user_id = $1 AND NOT archived AND income = $2 AND ($3::bigint IS NULL OR account_id IS NULL OR account_id = $3)
		  AND (CASE WHEN kind = 'subscription' THEN ABS(amount - $4) <= GREATEST(1, 0.02 * amount)
		            ELSE amount <= 0 OR $4 BETWEEN 0.5 * amount AND 2 * amount END)
		ORDER BY ABS(amount - $4), id`, uid, typ == "income", nullInt(account), amount)
	if err != nil {
		return m, false, err
	}
	type cand struct {
		id               int64
		name, kind, freq string
		amount           float64
		next             time.Time
		anchor           int
		auto             bool
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var next string
		if err := rows.Scan(&c.id, &c.name, &c.kind, &c.amount, &c.freq, &next, &c.anchor, &c.auto); err != nil {
			rows.Close()
			return m, false, err
		}
		c.next, _ = time.Parse("2006-01-02", next)
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return m, false, err
	}

	for _, c := range cands {
		// A cycle Sajni posted by itself near this day gives way to the real
		// payment: its row goes, this one takes its place.
		var payID, autoTxn int64
		var autoDue string
		err := tx.QueryRowContext(ctx, `SELECT p.id, p.due_date::text, l.txn_id FROM fin_biller_payments p
			JOIN fin_biller_payment_txns l ON l.payment_id = p.id
			WHERE p.biller_id = $1 AND p.auto AND $2::date BETWEEN p.due_date - $3::int AND p.due_date + $4::int
			ORDER BY ABS($2::date - p.due_date) LIMIT 1`, c.id, day, RecurringBefore, RecurringAfter).Scan(&payID, &autoDue, &autoTxn)
		if err == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2`, autoTxn, uid); err != nil {
				return m, false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE fin_biller_payments SET auto = FALSE, paid_date = $2, amount = $3 WHERE id = $1`, payID, day, amount); err != nil {
				return m, false, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO fin_biller_payment_txns (payment_id, txn_id) VALUES ($1,$2)`, payID, txnID); err != nil {
				return m, false, err
			}
			m = RecurringMatch{ID: c.id, Name: c.name, Due: autoDue}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return m, false, err
		} else {
			if dayT.Before(c.next.AddDate(0, 0, -RecurringBefore)) {
				continue // before the current cycle's window: paid already, or early
			}
			cycle := RecurringCycleOn(c.next, c.freq, c.anchor, dayT)
			if dayT.After(cycle.AddDate(0, 0, RecurringAfter)) {
				continue // between two windows
			}
			paid, err := PayRecurring(ctx, tx, uid, c.id, RecurringPay{DueDate: cycle.Format("2006-01-02"), PaidDate: day, Amount: &amount, AttachTxnIDs: []int64{txnID}})
			if err != nil {
				return m, false, err
			}
			if paid.AlreadyPaid {
				continue
			}
			m = RecurringMatch{ID: c.id, Name: c.name, Due: cycle.Format("2006-01-02")}
		}
		// The item names the row when capture chose its title; a row in
		// Unknown takes the item's category.
		if captured {
			if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions t SET description = b.name,
					category_id = COALESCE(b.category_id, t.category_id)
				FROM fin_billers b WHERE b.id = $1 AND t.id = $2 AND t.user_id = $3`, c.id, txnID, uid); err != nil {
				return m, false, err
			}
		} else if unknown {
			if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions t SET category_id = b.category_id
				FROM fin_billers b WHERE b.id = $1 AND b.category_id IS NOT NULL AND t.id = $2 AND t.user_id = $3`, c.id, txnID, uid); err != nil {
				return m, false, err
			}
		}
		tx.QueryRowContext(ctx, `SELECT description FROM fin_transactions WHERE id = $1`, txnID).Scan(&m.Title)
		return m, true, nil
	}
	return m, false, nil
}

func nullInt(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}
