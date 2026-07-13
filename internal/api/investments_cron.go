package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
)

// Investment auto-debit: for fin_investments rows with auto_debit on, the
// hourly tick posts the per-cycle contribution as an expense txn from the
// linked account, bumps invested_amount/current_value, and advances
// next_debit_date. fin_investment_contributions (uniq on investment_id,
// due_date) is the idempotency gate, mirroring fin_biller_payments.

// defaultNextDebitDate projects start_date's day-of-month to the next
// occurrence on or after today; with no usable start date it's today.
func defaultNextDebitDate(startDate *string, now time.Time) string {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if startDate == nil || *startDate == "" {
		return today.Format("2006-01-02")
	}
	start, err := time.Parse("2006-01-02", *startDate)
	if err != nil {
		return today.Format("2006-01-02")
	}
	if !start.Before(today) {
		return start.Format("2006-01-02")
	}
	cand := time.Date(today.Year(), today.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	if cand.Before(today) {
		cand = cand.AddDate(0, 1, 0)
	}
	return cand.Format("2006-01-02")
}

// advanceDebitDate rolls a debit date forward by one contribution cycle.
func advanceDebitDate(d time.Time, freq string) time.Time {
	switch freq {
	case "quarterly":
		return d.AddDate(0, 3, 0)
	case "yearly":
		return d.AddDate(1, 0, 0)
	default: // monthly
		return d.AddDate(0, 1, 0)
	}
}

// ProcessInvestmentDebits catches up every past-due auto-debit cycle. Each
// cycle: contribution row first (ON CONFLICT = already posted, skip), then
// the expense txn, then the investment totals. System txns take no pocket
// (pocket_id stays NULL — ambient money is General by definition).
func ProcessInvestmentDebits(ctx context.Context, deps Deps) (posted int, err error) {
	d := deps.DB
	// All Sajni users are IST; compare due dates against the shared zone.
	today := time.Now().In(defaultLoc).Format("2006-01-02")
	todayT, _ := time.Parse("2006-01-02", today)

	rows, err := d.QueryContext(ctx, `SELECT id, user_id, name, account_id, monthly_amount, frequency, next_debit_date::text
		FROM fin_investments
		WHERE auto_debit = TRUE AND account_id IS NOT NULL AND next_debit_date IS NOT NULL AND monthly_amount > 0`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type inv struct {
		id        int64
		userID    string
		name      string
		accountID int64
		amount    float64
		freq      string
		debitDate string
	}
	var invs []inv
	for rows.Next() {
		var i inv
		rows.Scan(&i.id, &i.userID, &i.name, &i.accountID, &i.amount, &i.freq, &i.debitDate)
		invs = append(invs, i)
	}

	for _, i := range invs {
		due, perr := time.Parse("2006-01-02", i.debitDate)
		if perr != nil {
			continue
		}
		cycles := 0
		for !due.After(todayT) {
			ok, derr := postInvestmentDebit(ctx, deps, i.userID, i.id, i.accountID, i.name, i.amount, due.Format("2006-01-02"))
			if derr != nil {
				log.Warn().Err(derr).Int64("investment", i.id).Msg("investment auto-debit failed")
				break
			}
			if ok {
				posted++
				cycles++
				notifyInvestmentDebit(ctx, deps, i.userID, i.name, i.amount,
					advanceDebitDate(due, i.freq).Format("2006-01-02"))
			}
			due = advanceDebitDate(due, i.freq)
		}
		d.ExecContext(ctx, `UPDATE fin_investments SET next_debit_date = $1, last_updated = NOW() WHERE id = $2`,
			due.Format("2006-01-02"), i.id)
		_ = cycles
	}
	return posted, nil
}

// postInvestmentDebit posts one contribution cycle inside a single DB txn,
// contribution-row-first so the UNIQUE key gates the money movement. Returns
// false when the cycle was already posted.
func postInvestmentDebit(ctx context.Context, deps Deps, uid string, invID, accountID int64,
	name string, amount float64, dueDate string,
) (bool, error) {
	tx, err := deps.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var contribID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO fin_investment_contributions
		(user_id, investment_id, due_date, amount, auto)
		VALUES ($1,$2,$3,$4,TRUE)
		ON CONFLICT (investment_id, due_date) DO NOTHING RETURNING id`,
		uid, invID, dueDate, amount).Scan(&contribID)
	if err != nil {
		// ErrNoRows = conflict swallowed the insert → cycle already posted.
		if errors.Is(err, sql.ErrNoRows) {
			return false, tx.Commit()
		}
		return false, err
	}

	var txnID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at)
		 VALUES ($1,$2,'expense',$3,$4,($5::timestamp AT TIME ZONE 'Asia/Kolkata')) RETURNING id`,
		uid, accountID, amount, name+" (auto)", dueDate).Scan(&txnID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE fin_investment_contributions SET txn_id = $1 WHERE id = $2`, txnID, contribID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_investments
		SET invested_amount = invested_amount + $1, current_value = current_value + $1, last_updated = NOW()
		WHERE id = $2 AND user_id = $3`, amount, invID, uid); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// notifyInvestmentDebit tells the user a contribution was auto-debited.
// Both channels per users.notify_channel (same contract as biller notifies):
// push via notifyPush, email unless a push-only user's push landed.
func notifyInvestmentDebit(ctx context.Context, deps Deps, uid, name string, amount float64, nextDate string) {
	pushed := notifyPush(ctx, deps, uid, push.Notification{
		Title: "Invested in " + name,
		Body:  fmt.Sprintf("₹%.2f auto-debited — next on %s", amount, nextDate),
		Route: "/finance/investments",
	})
	if deps.Auth == nil {
		return
	}
	var email, uname, channel string
	if err := deps.DB.QueryRowContext(ctx, `SELECT email, name, COALESCE(notify_channel,'both') FROM users WHERE id = $1`, uid).Scan(&email, &uname, &channel); err != nil || email == "" {
		return
	}
	if !channelWantsEmail(channel, pushed) {
		return
	}
	if uname == "" {
		uname = email
	}
	subject := "Auto-debited ₹" + strconv.FormatFloat(amount, 'f', 2, 64) + " for " + name
	html := "<p>Hi " + uname + ",</p>" +
		"<p>Your recurring investment <strong>" + name + "</strong> was auto-debited " +
		"<strong>₹" + strconv.FormatFloat(amount, 'f', 2, 64) + "</strong>. " +
		"The next contribution is scheduled for <strong>" + nextDate + "</strong>.</p>" +
		"<p>— Sajni</p>"
	if err := deps.Auth.SendEmail(ctx, email, subject, html); err != nil {
		log.Warn().Err(err).Str("investment", name).Msg("investment auto-debit email failed")
	}
}
