package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
	"sajni/internal/routine"
)

// Routines live in Journal: a day shows its routines in time order, each
// with a standing checklist and that day's agenda tasks. Logic is shared
// with the AI tools in internal/routine.
func registerRoutineRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/routines", listRoutines(deps))
	mux.HandleFunc("GET /api/routines/day", routinesForDay(deps))
	mux.HandleFunc("POST /api/routines", saveRoutine(deps, false))
	mux.HandleFunc("PUT /api/routines/{id}", saveRoutine(deps, true))
	mux.HandleFunc("DELETE /api/routines/{id}", deleteRoutine(deps))
	mux.HandleFunc("POST /api/routines/{id}/check", checkRoutineItem(deps))
}

func writeRoutineError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, routine.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		errJSON(w, 404, "routine not found")
	case errors.Is(err, routine.ErrInvalid):
		errJSON(w, 400, strings.TrimPrefix(err.Error(), routine.ErrInvalid.Error()+": "))
	default:
		internalError(w, r, "routine", err)
	}
}

func listRoutines(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := routine.List(r.Context(), deps.DB, userID(r.Context()))
		if err != nil {
			writeRoutineError(w, r, err)
			return
		}
		writeJSON(w, 200, items)
	}
}

func routinesForDay(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		date := queryParam(r, "date")
		if date == "" {
			date = userNow(deps.DB, uid).Format("2006-01-02")
		}
		items, err := routine.Day(r.Context(), deps.DB, uid, date)
		if err != nil {
			writeRoutineError(w, r, err)
			return
		}
		writeJSON(w, 200, items)
	}
}

func saveRoutine(deps Deps, existing bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var id int64
		if existing {
			var err error
			if id, err = intParam(r, "id"); err != nil {
				errJSON(w, 400, "invalid id")
				return
			}
		}
		var in routine.SaveInput
		if err := readJSON(r, &in); err != nil {
			errJSON(w, 400, "invalid routine")
			return
		}
		item, err := routine.Save(r.Context(), deps.DB, deps.ReminderQueue, userID(r.Context()), id, in)
		if err != nil {
			writeRoutineError(w, r, err)
			return
		}
		status := 200
		if !existing {
			status = 201
		}
		writeJSON(w, status, item)
	}
}

func deleteRoutine(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		if err := routine.Delete(r.Context(), deps.DB, userID(r.Context()), id); err != nil {
			writeRoutineError(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "deleted": true})
	}
}

func checkRoutineItem(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Date   string `json:"date"`
			ItemID string `json:"item_id"`
			Done   bool   `json:"done"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid check")
			return
		}
		if err := routine.Check(r.Context(), deps.DB, userID(r.Context()), id, body.Date, body.ItemID, body.Done); err != nil {
			writeRoutineError(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "item_id": body.ItemID, "done": body.Done})
	}
}

// processRoutineAlertCron is the daily safety net for Cloud Tasks: it sends
// alerts that came due without a fire and re-arms stale ones.
func processRoutineAlertCron(ctx context.Context, deps Deps) (int, error) {
	rows, err := deps.DB.QueryContext(ctx, `
		SELECT id FROM routines
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
		ok, err := sendRoutineAlert(ctx, deps, id)
		if err != nil {
			log.Warn().Err(err).Int64("routine", id).Msg("routine alert failed")
			continue
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// sendRoutineAlert sends one routine's start alert ("Office start · 9:00",
// its open items) over the user's notify channel, then advances the alert
// to the next occurrence. A stale alert (missed by more than the grace) is
// not sent late; it just moves on.
func sendRoutineAlert(ctx context.Context, deps Deps, id int64) (bool, error) {
	if deps.Auth == nil && deps.Push == nil {
		return false, nil
	}
	var name, start, uid, email, uname, channel string
	var itemsRaw []byte
	var at time.Time
	err := deps.DB.QueryRowContext(ctx, `
		UPDATE routines r SET alert_claimed_until = NOW() + make_interval(secs => $2)
		  FROM users u
		 WHERE r.id = $1 AND u.id = r.user_id AND u.deleted_at IS NULL
		   AND r.notify AND r.next_alert_at IS NOT NULL AND r.next_alert_at <= NOW()
		   AND (r.alert_claimed_until IS NULL OR r.alert_claimed_until < NOW())
		RETURNING r.name, r.start_time, r.items, r.next_alert_at, u.id::text, u.email, u.name,
		          COALESCE(u.notify_channel, 'both')`,
		id, int(reminderClaimLease.Seconds())).
		Scan(&name, &start, &itemsRaw, &at, &uid, &email, &uname, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if time.Since(at) > reminderGrace {
		return false, routine.Advance(ctx, deps.DB, deps.ReminderQueue, id, time.Now())
	}

	loc := routine.Location(ctx, deps.DB, uid)
	date := at.In(loc).Format("2006-01-02")
	var lines []string
	for _, it := range decodeRoutineItems(itemsRaw) {
		lines = append(lines, it.Text)
	}
	if rows, err := deps.DB.QueryContext(ctx, `
		SELECT title FROM tasks WHERE routine_id = $1 AND due_date = $2 AND status NOT IN ('done','scratched')
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
	body := routineAlertBody(lines)
	route := "/journal?date=" + date

	pushed := notifyPush(ctx, deps, uid, push.Notification{Type: push.TypeRoutine, Title: title, Body: body, Route: route})
	delivered := pushed
	if deps.Auth != nil && channelWantsEmail(channel, pushed) {
		if uname == "" {
			uname = email
		}
		if err := deps.Auth.SendTaskReminder(ctx, email, uname, name+": "+body, formatReminderWhen(at, loc.String()), route); err != nil {
			log.Warn().Err(err).Int64("routine", id).Msg("routine alert email failed")
		} else {
			delivered = true
		}
	}
	if !delivered {
		_, _ = deps.DB.ExecContext(ctx, `UPDATE routines SET alert_claimed_until = NULL WHERE id = $1`, id)
		return false, errors.New("no routine alert channel succeeded")
	}
	return true, routine.Advance(ctx, deps.DB, deps.ReminderQueue, id, at)
}

// routineAlertBody names the first few things to do: "Check calendar ·
// Standup · Reply to Rahul +2 more".
func routineAlertBody(lines []string) string {
	if len(lines) == 0 {
		return "Time to start."
	}
	const show = 3
	if len(lines) <= show {
		return strings.Join(lines, " · ")
	}
	return strings.Join(lines[:show], " · ") + " +" + itoa(len(lines)-show) + " more"
}

func decodeRoutineItems(raw []byte) []routine.Item {
	var items []routine.Item
	_ = json.Unmarshal(raw, &items)
	return items
}
