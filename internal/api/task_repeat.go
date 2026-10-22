package api

import (
	"context"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
	"sajni/internal/taskrepeat"
)

// ProcessRepeatingTasks moves repeating tasks whose day (or week, or month)
// has ended to their next date, and tells the user about any that moved
// while still blocked. Runs in the quarter-hour notifications sweep.
func ProcessRepeatingTasks(ctx context.Context, deps Deps) (int, error) {
	moved, err := taskrepeat.Sweep(ctx, deps.DB, deps.ReminderQueue)
	if err != nil {
		return 0, err
	}
	for _, m := range moved {
		if m.Blocked {
			notifyStillBlocked(ctx, deps, m)
		}
	}
	return len(moved), nil
}

// notifyStillBlocked: "Weekly report is still blocked · Missed Mon 20 Oct,
// next Mon 27 Oct", over the user's notify channel.
func notifyStillBlocked(ctx context.Context, deps Deps, m taskrepeat.Moved) {
	var email, name, channel string
	if err := deps.DB.QueryRowContext(ctx,
		`SELECT email, COALESCE(name,''), COALESCE(notify_channel,'both') FROM users WHERE id=$1 AND deleted_at IS NULL`, m.UserID).
		Scan(&email, &name, &channel); err != nil {
		return
	}
	label := func(date string) string {
		t, err := time.Parse("2006-01-02", date)
		if err != nil {
			return date
		}
		return t.Format("Mon 2 Jan")
	}
	title := m.Title + " is still blocked"
	body := "Missed " + label(m.From) + ", next " + label(m.To)
	route := "/tasks?focus=" + strconv.FormatInt(m.ID, 10)
	pushed := notifyPush(ctx, deps, m.UserID, push.Notification{Type: push.TypeTaskReminder, Title: title, Body: body, Route: route})
	if deps.Auth != nil && channelWantsEmail(channel, pushed) {
		if name == "" {
			name = email
		}
		if err := deps.Auth.SendTaskReminder(ctx, email, name, title, body, route); err != nil {
			log.Warn().Err(err).Int64("task", m.ID).Msg("still-blocked email failed")
		}
	}
}
