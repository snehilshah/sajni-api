package api

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
)

// Weekly & monthly task digests. Week/month tasks carry no scheduled_at, so
// the scheduled-time single-task reminder never fires for them. Instead a once-a-day
// Cloud Scheduler sweep (10:00 IST) hits /internal/reminders/digest; on a
// Friday it emails each user their still-pending week tasks, and on the last
// calendar day of the month their still-pending month tasks.
//
// Cycle model (Friday→Friday, month-end→month-end): a task is eligible when it
// has not been digested since the current day's local midnight boundary, so a
// still-pending task is nudged once per cycle (a recurring nag until done) and
// a task added after a fire is picked up on the next cycle. week_of/month_of
// <= the current period anchor keeps future-dated tasks out until their period
// begins, while overdue-but-pending ones keep surfacing. digested_at is the
// idempotency stamp (NULL = never digested).

// RegisterDigestCronHandler mounts the digest webhook. Header X-Reminder-Cron
// must match REMINDER_CRON_SECRET (shared with the other reminder webhooks).
func RegisterDigestCronHandler(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /internal/reminders/digest", digestCronHandler(deps))
}

func digestCronHandler(deps Deps) http.HandlerFunc {
	expected := os.Getenv("REMINDER_CRON_SECRET")
	return func(w http.ResponseWriter, r *http.Request) {
		if expected == "" || r.Header.Get("X-Reminder-Cron") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		week, month, err := ProcessDigestCron(r.Context(), deps)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, 200, map[string]int{"weekly": week, "monthly": month})
	}
}

// ProcessDigestCron sends the weekly digest on Fridays and the monthly digest
// on the last calendar day of the month, both evaluated in defaultLoc (every
// Sajni user is IST). Safe to call any day: it no-ops when neither applies, so
// a daily scheduler tick is all that's needed.
func ProcessDigestCron(ctx context.Context, deps Deps) (weekly, monthly int, err error) {
	if deps.Auth == nil && deps.Push == nil {
		return 0, 0, nil // no delivery channel configured
	}
	now := time.Now().In(defaultLoc)
	// Day boundary (local midnight): digested_at before this == eligible again.
	boundary := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, defaultLoc)

	if now.Weekday() == time.Friday {
		weekly = processWeeklyDigests(ctx, deps, now, boundary)
	}
	if isLastDayOfMonth(now) {
		monthly = processMonthlyDigests(ctx, deps, now, boundary)
	}
	return weekly, monthly, nil
}

// isLastDayOfMonth reports whether t is the final calendar day of its month.
func isLastDayOfMonth(t time.Time) bool {
	return t.AddDate(0, 0, 1).Month() != t.Month()
}

type digestRow struct {
	id    int64
	title string
}

type userDigest struct {
	uid, email, name string
	tasks            []digestRow
}

// processWeeklyDigests emails each user their pending week tasks (week_of <=
// this Monday) and stamps digested_at. periodLabel reads e.g. "Jun 16–22".
func processWeeklyDigests(ctx context.Context, deps Deps, now, boundary time.Time) int {
	monday := mondayOf(now)
	mondayKey := monday.Format("2006-01-02")
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT t.id, t.title, u.id, u.email, COALESCE(u.name,'')
		  FROM tasks t
		  JOIN users u ON u.id = t.user_id
		 WHERE t.week_of IS NOT NULL
		   AND t.week_of <= $1
		   AND t.status NOT IN ('done','scratched')
		   AND (t.digested_at IS NULL OR t.digested_at < $2)
		   AND u.deleted_at IS NULL
		 ORDER BY u.id, t.week_of, t.id`,
		mondayKey, boundary)
	if err != nil {
		log.Warn().Err(err).Msg("weekly digest query failed")
		return 0
	}
	users := scanDigestRows(rows)
	rows.Close()

	periodLabel := monday.Format("Jan 2") + "–" + monday.AddDate(0, 0, 6).Format("Jan 2")
	return deliverDigests(ctx, deps, users, "week", periodLabel)
}

// processMonthlyDigests emails each user their pending month tasks (month_of <=
// this month's 1st) and stamps digested_at. periodLabel reads e.g. "June 2026".
func processMonthlyDigests(ctx context.Context, deps Deps, now, boundary time.Time) int {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, defaultLoc)
	firstKey := first.Format("2006-01-02")
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT t.id, t.title, u.id, u.email, COALESCE(u.name,'')
		  FROM tasks t
		  JOIN users u ON u.id = t.user_id
		 WHERE t.month_of IS NOT NULL
		   AND t.month_of <= $1
		   AND t.status NOT IN ('done','scratched')
		   AND (t.digested_at IS NULL OR t.digested_at < $2)
		   AND u.deleted_at IS NULL
		 ORDER BY u.id, t.month_of, t.id`,
		firstKey, boundary)
	if err != nil {
		log.Warn().Err(err).Msg("monthly digest query failed")
		return 0
	}
	users := scanDigestRows(rows)
	rows.Close()

	return deliverDigests(ctx, deps, users, "month", first.Format("January 2006"))
}

// scanDigestRows folds the (task × user) join into one userDigest per user,
// preserving row order so the email lists tasks oldest-period first.
func scanDigestRows(rows interface {
	Next() bool
	Scan(...any) error
}) []*userDigest {
	var out []*userDigest
	byUID := map[string]*userDigest{}
	for rows.Next() {
		var id int64
		var title, uid, email, name string
		if err := rows.Scan(&id, &title, &uid, &email, &name); err != nil {
			continue
		}
		u := byUID[uid]
		if u == nil {
			u = &userDigest{uid: uid, email: email, name: name}
			byUID[uid] = u
			out = append(out, u)
		}
		u.tasks = append(u.tasks, digestRow{id: id, title: title})
	}
	return out
}

// deliverDigests sends one email + one summary push per user, then stamps every
// included task's digested_at. A failed delivery leaves the stamp unset so the
// next cycle retries. kind is "week" | "month".
func deliverDigests(ctx context.Context, deps Deps, users []*userDigest, kind, periodLabel string) int {
	sent := 0
	for _, u := range users {
		if len(u.tasks) == 0 {
			continue
		}
		titles := make([]string, len(u.tasks))
		ids := make([]int64, len(u.tasks))
		for i, t := range u.tasks {
			titles[i] = t.title
			ids[i] = t.id
		}
		name := u.name
		if name == "" {
			name = u.email
		}

		pushed := notifyPush(ctx, deps, u.uid, push.Notification{
			Title: digestPushTitle(kind, len(titles)),
			Body:  periodLabel,
			Route: "/tasks",
		})

		emailed := false
		if deps.Auth != nil {
			if err := deps.Auth.SendTaskDigest(ctx, u.email, name, kind, periodLabel, titles, "/tasks"); err != nil {
				log.Warn().Err(err).Str("user", u.uid).Str("kind", kind).Msg("digest email failed")
			} else {
				emailed = true
			}
		}
		if !pushed && !emailed {
			continue // nothing delivered — leave digested_at unset to retry
		}
		if _, err := deps.DB.ExecContext(ctx,
			`UPDATE tasks SET digested_at = NOW() WHERE id = ANY($1)`, ids); err != nil {
			log.Warn().Err(err).Str("user", u.uid).Msg("digest stamp failed")
			continue
		}
		sent++
	}
	return sent
}

func digestPushTitle(kind string, n int) string {
	noun := "week"
	if kind == "month" {
		noun = "month"
	}
	if n == 1 {
		return "1 pending " + noun + " task"
	}
	return strconv.Itoa(n) + " pending " + noun + " tasks"
}
