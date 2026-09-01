package api

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
)

// ProcessLendReminders sends one opt-in reminder for the current due date.
// A changed due date clears the stamp in updateLend, making the new date
// eligible without producing repeated overdue notifications every 15 minutes.
func ProcessLendReminders(ctx context.Context, deps Deps) (int, error) {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT l.id, l.user_id, l.borrower, l.due_date::text,
		       GREATEST(l.principal - COALESCE(SUM(r.amount),0),0)
		FROM fin_lends l
		LEFT JOIN fin_lend_repayments r ON r.lend_id=l.id AND r.user_id=l.user_id
		WHERE l.remind AND l.due_date IS NOT NULL
		  AND l.last_reminded_due_date IS DISTINCT FROM l.due_date
		GROUP BY l.id
		HAVING GREATEST(l.principal - COALESCE(SUM(r.amount),0),0) > 0`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type dueLend struct {
		id          int64
		uid         string
		borrower    string
		dueDate     string
		outstanding float64
	}
	var due []dueLend
	for rows.Next() {
		var item dueLend
		if err := rows.Scan(&item.id, &item.uid, &item.borrower, &item.dueDate, &item.outstanding); err != nil {
			return 0, err
		}
		due = append(due, item)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, item := range due {
		now := userNow(deps.DB, item.uid)
		if !scheduledNotificationWindow(now) {
			continue
		}
		dueDate, err := time.Parse("2006-01-02", item.dueDate)
		if err != nil || dueDate.After(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)) {
			continue
		}
		claimed, err := deps.DB.ExecContext(ctx, `UPDATE fin_lends SET last_reminded_due_date=due_date
			WHERE id=$1 AND user_id=$2 AND last_reminded_due_date IS DISTINCT FROM due_date`, item.id, item.uid)
		if err != nil {
			return sent, err
		}
		if n, _ := claimed.RowsAffected(); n == 0 {
			continue
		}
		notifyLendDue(ctx, deps, item.uid, item.borrower, item.dueDate, item.outstanding)
		sent++
	}
	return sent, nil
}

func notifyLendDue(ctx context.Context, deps Deps, uid, borrower, dueDate string, outstanding float64) {
	body := fmt.Sprintf("₹%.2f remains outstanding · due %s", outstanding, dueDate)
	pushed := notifyPush(ctx, deps, uid, push.Notification{
		Type: push.TypeLendDue, Title: "Repayment due from " + borrower,
		Body: body, Route: "/finance/lends",
	})
	if deps.Auth == nil {
		return
	}
	var email, name, channel string
	if err := deps.DB.QueryRowContext(ctx, `SELECT email, name, COALESCE(notify_channel,'both') FROM users WHERE id=$1`, uid).Scan(&email, &name, &channel); err != nil || email == "" {
		return
	}
	if !channelWantsEmail(channel, pushed) {
		return
	}
	if name == "" {
		name = email
	}
	subject := "Repayment due from " + borrower
	markup := "<p>Hi " + html.EscapeString(name) + ",</p><p><strong>" +
		html.EscapeString(borrower) + "</strong> has <strong>₹" +
		strconv.FormatFloat(outstanding, 'f', 2, 64) + "</strong> outstanding, due " +
		html.EscapeString(dueDate) + ".</p><p>— Sajni</p>"
	if err := deps.Auth.SendEmail(ctx, email, subject, markup); err != nil {
		log.Warn().Err(err).Msg("lend reminder email failed")
	}
}
