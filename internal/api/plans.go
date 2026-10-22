package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/plan"
	"sajni/internal/push"
)

// Plans live in Journal: a day shows its plans in time order, each with the
// tasks planned for it. Logic is shared with the AI tools in internal/plan.
func registerPlanRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/plans", listPlans(deps))
	mux.HandleFunc("GET /api/plans/day", plansForDay(deps))
	mux.HandleFunc("GET /api/plans/{id}/candidates", planCandidates(deps))
	mux.HandleFunc("POST /api/plans/{id}/tasks", withSync(deps, "tasks", addPlanTask(deps)))
	mux.HandleFunc("POST /api/plans", savePlan(deps, false))
	mux.HandleFunc("PUT /api/plans/{id}", savePlan(deps, true))
	mux.HandleFunc("DELETE /api/plans/{id}", deletePlan(deps))
}

func writePlanError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, plan.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		errJSON(w, 404, "plan not found")
	case errors.Is(err, plan.ErrInvalid):
		errJSON(w, 400, strings.TrimPrefix(err.Error(), plan.ErrInvalid.Error()+": "))
	default:
		internalError(w, r, "plan", err)
	}
}

func listPlans(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := plan.List(r.Context(), deps.DB, userID(r.Context()))
		if err != nil {
			writePlanError(w, r, err)
			return
		}
		writeJSON(w, 200, items)
	}
}

func plansForDay(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		date := queryParam(r, "date")
		if date == "" {
			date = userNow(deps.DB, uid).Format("2006-01-02")
		}
		items, err := plan.Day(r.Context(), deps.DB, uid, date)
		if err != nil {
			writePlanError(w, r, err)
			return
		}
		writeJSON(w, 200, items)
	}
}

func planCandidates(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		date := queryParam(r, "date")
		if date == "" {
			date = userNow(deps.DB, uid).Format("2006-01-02")
		}
		groups, err := plan.Candidates(r.Context(), deps.DB, uid, id, date)
		if err != nil {
			writePlanError(w, r, err)
			return
		}
		writeJSON(w, 200, groups)
	}
}

// addPlanTask pulls an existing task into the plan's occurrence on date.
func addPlanTask(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			TaskID int64  `json:"task_id"`
			Date   string `json:"date"`
		}
		if err := readJSON(r, &body); err != nil || body.TaskID == 0 {
			errJSON(w, 400, "task_id is required")
			return
		}
		date, err := plan.AddTask(r.Context(), deps.DB, deps.ReminderQueue, userID(r.Context()), id, body.TaskID, body.Date)
		if err != nil {
			writePlanError(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "task_id": body.TaskID, "date": date})
	}
}

func savePlan(deps Deps, existing bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var id int64
		if existing {
			var err error
			if id, err = intParam(r, "id"); err != nil {
				errJSON(w, 400, "invalid id")
				return
			}
		}
		var in plan.SaveInput
		if err := readJSON(r, &in); err != nil {
			errJSON(w, 400, "invalid plan")
			return
		}
		item, err := plan.Save(r.Context(), deps.DB, deps.ReminderQueue, userID(r.Context()), id, in)
		if err != nil {
			writePlanError(w, r, err)
			return
		}
		status := 200
		if !existing {
			status = 201
		}
		writeJSON(w, status, item)
	}
}

func deletePlan(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		if err := plan.Delete(r.Context(), deps.DB, userID(r.Context()), id); err != nil {
			writePlanError(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "deleted": true})
	}
}

// processPlanAlertCron is the daily safety net for Cloud Tasks: it sends
// alerts that came due without a fire and re-arms stale ones.
func processPlanAlertCron(ctx context.Context, deps Deps) (int, error) {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT id FROM plans
		 WHERE notify AND next_alert_at <= NOW()
		   AND (alert_claimed_until IS NULL OR alert_claimed_until < NOW())
		 ORDER BY next_alert_at LIMIT 500`)
	if err != nil {
		return 0, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	sent := 0
	for _, id := range ids {
		ok, err := sendPlanAlert(ctx, deps, id)
		if err != nil {
			log.Warn().Err(err).Int64("plan", id).Msg("plan alert failed")
			continue
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// sendPlanAlert sends one plan's start alert ("Office start · 9:00 AM",
// what's planned, or a nudge to plan) over the user's notify channel, then
// advances the alert to the next occurrence. A stale alert (missed by more
// than the grace) is not sent late; it just moves on.
func sendPlanAlert(ctx context.Context, deps Deps, id int64) (bool, error) {
	if deps.Auth == nil && deps.Push == nil {
		return false, nil
	}
	var name, uid, email, uname, channel string
	var at time.Time
	err := deps.DB.QueryRowContext(ctx, `
		UPDATE plans p SET alert_claimed_until = NOW() + make_interval(secs => $2)
		  FROM users u
		 WHERE p.id = $1 AND u.id = p.user_id AND u.deleted_at IS NULL
		   AND p.notify AND p.next_alert_at IS NOT NULL AND p.next_alert_at <= NOW()
		   AND (p.alert_claimed_until IS NULL OR p.alert_claimed_until < NOW())
		RETURNING p.name, p.next_alert_at, u.id::text, u.email, u.name, COALESCE(u.notify_channel, 'both')`,
		id, int(reminderClaimLease.Seconds())).
		Scan(&name, &at, &uid, &email, &uname, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if time.Since(at) > reminderGrace {
		return false, plan.Advance(ctx, deps.DB, deps.ReminderQueue, id, time.Now())
	}

	loc := plan.Location(ctx, deps.DB, uid)
	date := at.In(loc).Format("2006-01-02")
	var lines []string
	if rows, err := deps.DB.QueryContext(ctx, `
		SELECT title FROM tasks WHERE plan_id = $1 AND due_date = $2 AND status NOT IN ('done','scratched')
		 ORDER BY created_at, id`, id, date); err == nil {
		for rows.Next() {
			var t string
			if rows.Scan(&t) == nil {
				lines = append(lines, t)
			}
		}
		rows.Close()
	}
	title := name + " · " + at.In(loc).Format("3:04 PM")
	body := planAlertBody(lines)
	route := "/journal?date=" + date

	pushed := notifyPush(ctx, deps, uid, push.Notification{Type: push.TypePlan, Title: title, Body: body, Route: route})
	delivered := pushed
	if deps.Auth != nil && channelWantsEmail(channel, pushed) {
		if uname == "" {
			uname = email
		}
		if err := deps.Auth.SendTaskReminder(ctx, email, uname, name+": "+body, formatReminderWhen(at, loc.String()), route); err != nil {
			log.Warn().Err(err).Int64("plan", id).Msg("plan alert email failed")
		} else {
			delivered = true
		}
	}
	if !delivered {
		_, _ = deps.DB.ExecContext(ctx, `UPDATE plans SET alert_claimed_until = NULL WHERE id = $1`, id)
		return false, errors.New("no plan alert channel succeeded")
	}
	return true, plan.Advance(ctx, deps.DB, deps.ReminderQueue, id, at)
}

// planAlertBody names what's planned: "Standup · Reply to Rahul +2 more",
// or asks for a plan when there's none yet.
func planAlertBody(lines []string) string {
	if len(lines) == 0 {
		return "Nothing planned yet. Plan your day."
	}
	const show = 3
	if len(lines) <= show {
		return strings.Join(lines, " · ")
	}
	return strings.Join(lines[:show], " · ") + " +" + itoa(len(lines)-show) + " more"
}
