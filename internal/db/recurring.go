package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
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

// RecurringState says how an item's row reads: "paid" (the cycle before next
// is paid and next's window hasn't opened; date = when it was paid),
// "not_recorded" (next is past, or it's its day after 8 pm; date = next) or
// "due" (date = next). lastDue/lastPaid are the newest payment's dates ("" =
// none); now is the user's local time.
func RecurringState(next time.Time, lastDue, lastPaid string, now time.Time) (string, string) {
	today, _ := time.Parse("2006-01-02", now.Format("2006-01-02"))
	nextS := next.Format("2006-01-02")
	if today.After(next) || (today.Equal(next) && now.Hour() >= 20) {
		return "not_recorded", nextS
	}
	if lastDue != "" && lastDue < nextS && today.Before(next.AddDate(0, 0, -RecurringBefore)) {
		return "paid", lastPaid
	}
	return "due", nextS
}

// UnlinkRecurringPayment undoes one recorded cycle (a payment Sajni attached
// by mistake, or a tap on the wrong item): the cycle is due again and its
// rows stay as they are. Undoing the newest cycle moves the due date back to
// it. Returns the item's next due date.
func UnlinkRecurringPayment(ctx context.Context, tx *sql.Tx, uid string, id, paymentID int64) (string, error) {
	var next string
	if err := tx.QueryRowContext(ctx, `SELECT next_due_date::text FROM fin_billers WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, uid).Scan(&next); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrRecurringNotFound
		}
		return "", err
	}
	var due string
	if err := tx.QueryRowContext(ctx, `DELETE FROM fin_biller_payments WHERE id = $1 AND biller_id = $2 AND user_id = $3 RETURNING due_date::text`,
		paymentID, id, uid).Scan(&due); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("that payment isn't on this item")
		}
		return "", err
	}
	var later bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_biller_payments WHERE biller_id = $1 AND due_date > $2)`, id, due).Scan(&later); err != nil {
		return "", err
	}
	if due < next && !later {
		if _, err := tx.ExecContext(ctx, `UPDATE fin_billers SET next_due_date = $1, updated_at = NOW() WHERE id = $2`, due, id); err != nil {
			return "", err
		}
		next = due
	}
	return next, nil
}

// RecurringSuggestion is something that looks like it repeats monthly but
// isn't an item yet ("ChatGPT Plus ₹1,950 on RuPay around the 23rd").
type RecurringSuggestion struct {
	Key         string   `json:"key"`
	Title       string   `json:"title"`
	Income      bool     `json:"income"`
	Amount      float64  `json:"amount"`
	AccountID   int64    `json:"account_id"`
	AccountName string   `json:"account_name"`
	CategoryID  *int64   `json:"category_id"`
	Day         int      `json:"day"`
	Months      []string `json:"months"` // "2026-10", oldest first
	NextDueDate string   `json:"next_due_date"`
}

// RecurringSuggestions finds titles paid (or received) once a month in at
// least two months running, the latest this month or last, on one account,
// within 10% and 5 days of each other, that no item covers and the owner
// hasn't dismissed. Anything that happens more than once in a month (a daily
// auto ride) never qualifies. Rows already attached to an item don't count.
func RecurringSuggestions(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, uid string) ([]RecurringSuggestion, error) {
	rows, err := q.QueryContext(ctx, `
		WITH u AS (SELECT COALESCE(NULLIF(timezone,''),'Asia/Kolkata') AS z FROM users WHERE id = $1),
		t AS (
			SELECT t.type = 'income' AS income, LOWER(BTRIM(t.description)) AS k, t.description, t.amount, t.account_id, a.name AS acct,
			       t.category_id, (t.txn_at AT TIME ZONE u.z)::date AS d
			FROM fin_transactions t JOIN fin_accounts a ON a.id = t.account_id, u
			WHERE t.user_id = $1 AND t.type IN ('expense','income') AND t.split_of IS NULL AND BTRIM(t.description) <> ''
			  AND (t.txn_at AT TIME ZONE u.z)::date >= date_trunc('month', (NOW() AT TIME ZONE u.z)::date) - INTERVAL '5 months'
			  AND NOT EXISTS (SELECT 1 FROM fin_biller_payment_txns l WHERE l.txn_id = t.id)
		)
		SELECT income, k, description, amount, account_id, acct, category_id, d::text,
		       (SELECT to_char(date_trunc('month', (NOW() AT TIME ZONE u.z)::date), 'YYYY-MM') FROM u)
		FROM t ORDER BY income, k, account_id, d`, uid)
	if err != nil {
		return nil, err
	}
	type hit struct {
		title  string
		amount float64
		cat    sql.NullInt64
		day    time.Time
	}
	type group struct {
		income bool
		k      string
		acct   int64
		name   string
		hits   []hit
	}
	var groups []*group
	byKey := map[string]*group{}
	var thisMonth string
	for rows.Next() {
		var g group
		var h hit
		var d string
		if err := rows.Scan(&g.income, &g.k, &h.title, &h.amount, &g.acct, &g.name, &h.cat, &d, &thisMonth); err != nil {
			rows.Close()
			return nil, err
		}
		h.day, _ = time.Parse("2006-01-02", d)
		key := fmt.Sprintf("%t:%s:%d", g.income, g.k, g.acct)
		if byKey[key] == nil {
			gg := g
			byKey[key] = &gg
			groups = append(groups, &gg)
		}
		byKey[key].hits = append(byKey[key].hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	dismissed := map[string]bool{}
	drows, err := q.QueryContext(ctx, `SELECT key FROM fin_recurring_dismissed WHERE user_id = $1`, uid)
	if err != nil {
		return nil, err
	}
	for drows.Next() {
		var k string
		drows.Scan(&k)
		dismissed[k] = true
	}
	drows.Close()
	type item struct {
		name   string
		income bool
		acct   sql.NullInt64
		amount float64
		anchor int
	}
	var items []item
	irows, err := q.QueryContext(ctx, `SELECT LOWER(BTRIM(name)), income, account_id, amount, COALESCE(anchor_day, EXTRACT(DAY FROM next_due_date)::INTEGER)
		FROM fin_billers WHERE user_id = $1 AND NOT archived`, uid)
	if err != nil {
		return nil, err
	}
	for irows.Next() {
		var it item
		irows.Scan(&it.name, &it.income, &it.acct, &it.amount, &it.anchor)
		items = append(items, it)
	}
	irows.Close()

	now, _ := time.Parse("2006-01", thisMonth)
	lastMonth := now.AddDate(0, -1, 0).Format("2006-01")
	out := []RecurringSuggestion{}
	for _, g := range groups {
		key := fmt.Sprintf("%t:%s:%d", g.income, g.k, g.acct)
		if dismissed[key] {
			continue
		}
		perMonth := map[string]int{}
		for _, h := range g.hits {
			perMonth[h.day.Format("2006-01")]++
		}
		once := true
		for _, n := range perMonth {
			if n > 1 {
				once = false
			}
		}
		if !once || len(g.hits) < 2 {
			continue
		}
		// The latest run of consecutive months, ending this month or last.
		last := g.hits[len(g.hits)-1]
		if m := last.day.Format("2006-01"); m != thisMonth && m != lastMonth {
			continue
		}
		run := []hit{last}
		for i := len(g.hits) - 2; i >= 0; i-- {
			if g.hits[i].day.Format("2006-01") != run[0].day.AddDate(0, 0, -run[0].day.Day()+1).AddDate(0, -1, 0).Format("2006-01") {
				break
			}
			run = append([]hit{g.hits[i]}, run...)
		}
		if len(run) < 2 {
			continue
		}
		lo, hi, dlo, dhi := run[0].amount, run[0].amount, run[0].day.Day(), run[0].day.Day()
		for _, h := range run {
			lo, hi = math.Min(lo, h.amount), math.Max(hi, h.amount)
			if h.day.Day() < dlo {
				dlo = h.day.Day()
			}
			if h.day.Day() > dhi {
				dhi = h.day.Day()
			}
		}
		if hi > lo*1.1 || dhi-dlo > 5 {
			continue
		}
		covered := false
		for _, it := range items {
			fits := it.income == g.income && (!it.acct.Valid || it.acct.Int64 == g.acct) &&
				math.Abs(it.amount-last.amount) <= math.Max(1, 0.1*it.amount) && absInt(it.anchor-last.day.Day()) <= 5
			if it.name == g.k || fits {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		s := RecurringSuggestion{
			Key: key, Title: last.title, Income: g.income, Amount: last.amount, AccountID: g.acct, AccountName: g.name,
			Day: last.day.Day(), NextDueDate: AdvanceDue(last.day, "monthly", last.day.Day()).Format("2006-01-02"),
		}
		if last.cat.Valid {
			c := last.cat.Int64
			s.CategoryID = &c
		}
		for _, h := range run {
			s.Months = append(s.Months, h.day.Format("2006-01"))
		}
		out = append(out, s)
	}
	return out, nil
}

// DismissRecurringSuggestion hides one suggestion for good ("It doesn't
// repeat").
func DismissRecurringSuggestion(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, uid, key string) error {
	if key == "" {
		return errors.New("missing key")
	}
	_, err := q.ExecContext(ctx, `INSERT INTO fin_recurring_dismissed (user_id, key) VALUES ($1,$2) ON CONFLICT DO NOTHING`, uid, key)
	return err
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
