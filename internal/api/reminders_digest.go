package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
)

// Weekly & monthly task digests. Week/month tasks carry no scheduled_at, so
// the scheduled-time single-task reminder never fires for them. The shared
// scheduled sweep runs every 15 minutes; each user is selected only during
// the 10:00 quarter-hour in their own timezone. Friday and month-end are also
// evaluated on that local calendar.
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
// on the last calendar day of the month at 10:00 in each user's timezone.
// Safe to call every 15 minutes: outside a user's local delivery window it
// performs no work.
func ProcessDigestCron(ctx context.Context, deps Deps) (weekly, monthly int, err error) {
	return processDigestCronAt(ctx, deps, time.Now())
}

func processDigestCronAt(ctx context.Context, deps Deps, now time.Time) (weekly, monthly int, err error) {
	if deps.Auth == nil && deps.Push == nil {
		return 0, 0, nil // no delivery channel configured
	}

	owners, err := listDigestOwners(ctx, deps)
	if err != nil {
		return 0, 0, err
	}
	var jobErrors []error
	for _, owner := range owners {
		localNow := now.In(timezoneLocation(owner.timezone))
		if !scheduledNotificationWindow(localNow) {
			continue
		}
		// Day boundary as an instant: digested_at before this is eligible for
		// the current local cycle, regardless of the database server timezone.
		boundary := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, localNow.Location())

		if localNow.Weekday() == time.Friday {
			n, weeklyErr := processWeeklyDigest(ctx, deps, owner, localNow, boundary)
			weekly += n
			if weeklyErr != nil {
				jobErrors = append(jobErrors, weeklyErr)
			}
		}
		if isLastDayOfMonth(localNow) {
			n, monthlyErr := processMonthlyDigest(ctx, deps, owner, localNow, boundary)
			monthly += n
			if monthlyErr != nil {
				jobErrors = append(jobErrors, monthlyErr)
			}
		}
	}
	return weekly, monthly, errors.Join(jobErrors...)
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
	channel          string
	timezone         string
	tasks            []digestRow
}

func listDigestOwners(ctx context.Context, deps Deps) ([]*userDigest, error) {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT id, email, COALESCE(name,''), COALESCE(notify_channel,'both'), COALESCE(timezone,'')
		  FROM users
		 WHERE deleted_at IS NULL
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query digest users: %w", err)
	}
	defer rows.Close()

	var owners []*userDigest
	for rows.Next() {
		owner := &userDigest{}
		if err := rows.Scan(&owner.uid, &owner.email, &owner.name, &owner.channel, &owner.timezone); err != nil {
			return nil, fmt.Errorf("scan digest user: %w", err)
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return owners, nil
}

// processWeeklyDigests emails each user their pending week tasks (week_of <=
// this Monday) and stamps digested_at. periodLabel reads e.g. "Jun 16–22".
func processWeeklyDigest(ctx context.Context, deps Deps, owner *userDigest, now, boundary time.Time) (int, error) {
	monday := mondayOf(now)
	mondayKey := monday.Format("2006-01-02")
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT t.id, t.title
		  FROM tasks t
		 WHERE t.user_id = $1
		   AND t.week_of IS NOT NULL
		   AND t.week_of <= $2
		   AND t.status NOT IN ('done','scratched')
		   AND (t.digested_at IS NULL OR t.digested_at < $3)
		 ORDER BY t.week_of, t.id`,
		owner.uid, mondayKey, boundary)
	if err != nil {
		return 0, fmt.Errorf("query weekly digests: %w", err)
	}
	defer rows.Close()
	owner.tasks = nil
	for rows.Next() {
		var task digestRow
		if err := rows.Scan(&task.id, &task.title); err != nil {
			return 0, fmt.Errorf("scan weekly digest: %w", err)
		}
		owner.tasks = append(owner.tasks, task)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	periodLabel := monday.Format("Jan 2") + "–" + monday.AddDate(0, 0, 6).Format("Jan 2")
	sent := deliverDigests(ctx, deps, []*userDigest{owner}, "week", periodLabel)
	return sent, nil
}

// processMonthlyDigests emails each user their pending month tasks (month_of <=
// this month's 1st) and stamps digested_at. periodLabel reads e.g. "June 2026".
func processMonthlyDigest(ctx context.Context, deps Deps, owner *userDigest, now, boundary time.Time) (int, error) {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	firstKey := first.Format("2006-01-02")
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT t.id, t.title
		  FROM tasks t
		 WHERE t.user_id = $1
		   AND t.month_of IS NOT NULL
		   AND t.month_of <= $2
		   AND t.status NOT IN ('done','scratched')
		   AND (t.digested_at IS NULL OR t.digested_at < $3)
		 ORDER BY t.month_of, t.id`,
		owner.uid, firstKey, boundary)
	if err != nil {
		return 0, fmt.Errorf("query monthly digests: %w", err)
	}
	defer rows.Close()
	owner.tasks = nil
	for rows.Next() {
		var task digestRow
		if err := rows.Scan(&task.id, &task.title); err != nil {
			return 0, fmt.Errorf("scan monthly digest: %w", err)
		}
		owner.tasks = append(owner.tasks, task)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := deliverDigests(ctx, deps, []*userDigest{owner}, "month", first.Format("January 2006"))
	return sent, nil
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
		if deps.Auth != nil && channelWantsEmail(u.channel, pushed) {
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
