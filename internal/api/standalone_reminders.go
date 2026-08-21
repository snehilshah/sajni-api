package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
	"sajni/internal/reminder"
)

func registerReminderRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/reminders", listStandaloneReminders(deps))
	mux.HandleFunc("GET /api/reminders/recent", listReminderHistory(deps))
	mux.HandleFunc("GET /api/reminders/{id}", getStandaloneReminder(deps))
	mux.HandleFunc("POST /api/reminders", createStandaloneReminder(deps))
	mux.HandleFunc("PUT /api/reminders/{id}", updateStandaloneReminder(deps))
	mux.HandleFunc("DELETE /api/reminders/{id}", deleteStandaloneReminder(deps))
	mux.HandleFunc("POST /api/reminders/{id}/snooze", snoozeStandaloneReminder(deps))
	mux.HandleFunc("POST /api/reminders/{id}/skip", skipStandaloneReminder(deps))
}

func listStandaloneReminders(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(queryParam(r, "limit"))
		items, err := reminder.List(r.Context(), deps.DB, userID(r.Context()), queryParam(r, "search"), queryParam(r, "all") != "true", limit)
		if err != nil {
			internalError(w, r, "list reminders", err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func listReminderHistory(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(queryParam(r, "limit"))
		offset, _ := strconv.Atoi(queryParam(r, "offset"))
		items, err := reminder.History(r.Context(), deps.DB, userID(r.Context()), limit, offset)
		if err != nil {
			internalError(w, r, "list reminder history", err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	}
}

func getStandaloneReminder(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		item, err := reminder.Get(r.Context(), deps.DB, userID(r.Context()), id)
		writeReminderResult(w, r, item, err, http.StatusOK)
	}
}

func createStandaloneReminder(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input reminder.SaveInput
		if err := readJSON(r, &input); err != nil {
			errJSON(w, 400, "invalid reminder")
			return
		}
		item, err := reminder.Create(r.Context(), deps.DB, deps.ReminderQueue, userID(r.Context()), input)
		writeReminderResult(w, r, item, err, http.StatusCreated)
	}
}

func updateStandaloneReminder(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var input reminder.SaveInput
		if err := readJSON(r, &input); err != nil {
			errJSON(w, 400, "invalid reminder")
			return
		}
		item, err := reminder.Replace(r.Context(), deps.DB, deps.ReminderQueue, userID(r.Context()), id, input)
		writeReminderResult(w, r, item, err, http.StatusOK)
	}
}

func deleteStandaloneReminder(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		if err := reminder.Delete(r.Context(), deps.DB, userID(r.Context()), id); err != nil {
			writeReminderResult(w, r, reminder.Reminder{}, err, http.StatusOK)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
	}
}

func snoozeStandaloneReminder(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			OccurrenceID int64  `json:"occurrence_id"`
			FireAt       string `json:"fire_at"`
			Minutes      int    `json:"minutes"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid snooze")
			return
		}
		var fireAt time.Time
		if body.FireAt != "" {
			fireAt, err = time.Parse(time.RFC3339, body.FireAt)
		} else if body.Minutes > 0 && body.Minutes <= 43200 {
			fireAt = time.Now().Add(time.Duration(body.Minutes) * time.Minute)
		} else {
			err = errors.New("fire_at or minutes required")
		}
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		item, err := reminder.Snooze(r.Context(), deps.DB, deps.ReminderQueue, userID(r.Context()), id, body.OccurrenceID, fireAt)
		writeReminderResult(w, r, item, err, http.StatusOK)
	}
}

func skipStandaloneReminder(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			OccurrenceID int64 `json:"occurrence_id"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid skip")
			return
		}
		item, err := reminder.Skip(r.Context(), deps.DB, deps.ReminderQueue, userID(r.Context()), id, body.OccurrenceID)
		writeReminderResult(w, r, item, err, http.StatusOK)
	}
}

func writeReminderResult(w http.ResponseWriter, r *http.Request, item reminder.Reminder, err error, status int) {
	if errors.Is(err, reminder.ErrNotFound) {
		errJSON(w, 404, "reminder not found")
		return
	}
	if err != nil {
		// Validation errors are intentionally plain; database failures retain
		// their detail only in structured server logs.
		if errors.Is(err, sql.ErrNoRows) {
			errJSON(w, 404, "reminder not found")
			return
		}
		if errors.Is(err, reminder.ErrInvalid) {
			errJSON(w, 400, strings.TrimPrefix(err.Error(), reminder.ErrInvalid.Error()+": "))
			return
		}
		internalError(w, r, "save reminder", err)
		return
	}
	writeJSON(w, status, item)
}

// processStandaloneReminderCron is the low-frequency safety net for Cloud
// Tasks delivery. Exact fires normally arrive through the queued occurrence.
func processStandaloneReminderCron(ctx context.Context, deps Deps) (int, error) {
	rows, err := deps.DB.QueryContext(ctx, `SELECT id FROM reminder_occurrences WHERE status='pending' AND fire_at<=NOW() AND fire_at>=NOW()-make_interval(secs=>$1) AND (claimed_until IS NULL OR claimed_until<NOW()) ORDER BY fire_at LIMIT 200`, int(reminderGrace.Seconds()))
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
		ok, err := sendStandaloneReminder(ctx, deps, id)
		if err != nil {
			log.Warn().Err(err).Int64("occurrence", id).Msg("standalone reminder delivery failed")
			continue
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

func sendStandaloneReminder(ctx context.Context, deps Deps, occurrenceID int64) (bool, error) {
	if deps.Auth == nil && deps.Push == nil {
		return false, nil
	}
	type due struct {
		reminderID int64
		message    string
		fireAt     time.Time
		uid        string
		email      string
		name       string
		timezone   string
		channel    string
	}
	var item due
	err := deps.DB.QueryRowContext(ctx, `
		WITH claimed AS (
			UPDATE reminder_occurrences o SET claimed_until=NOW()+make_interval(secs=>$2)
			FROM reminders r
			WHERE o.id=$1 AND r.id=o.reminder_id AND r.active=TRUE AND o.status='pending'
			  AND o.fire_at<=NOW() AND o.fire_at>=NOW()-make_interval(secs=>$3)
			  AND (o.claimed_until IS NULL OR o.claimed_until<NOW())
			RETURNING o.reminder_id,o.user_id,o.fire_at
		)
		SELECT c.reminder_id,r.message,c.fire_at,u.id,u.email,u.name,COALESCE(u.timezone,r.timezone),COALESCE(u.notify_channel,'both')
		FROM claimed c JOIN reminders r ON r.id=c.reminder_id JOIN users u ON u.id=c.user_id
		WHERE u.deleted_at IS NULL`, occurrenceID, int(reminderClaimLease.Seconds()), int(reminderGrace.Seconds())).
		Scan(&item.reminderID, &item.message, &item.fireAt, &item.uid, &item.email, &item.name, &item.timezone, &item.channel)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	route := fmt.Sprintf("/tasks?tab=reminders&focus=%d", item.reminderID)
	pushed := notifyPush(ctx, deps, item.uid, push.Notification{
		Type:     push.TypeReminder,
		Title:    "Reminder",
		Body:     item.message,
		Route:    route,
		DataOnly: true,
		Data: map[string]string{
			"reminder_id":   strconv.FormatInt(item.reminderID, 10),
			"occurrence_id": strconv.FormatInt(occurrenceID, 10),
		},
	})
	delivered := pushed
	if deps.Auth != nil && channelWantsEmail(item.channel, pushed) {
		name := item.name
		if name == "" {
			name = item.email
		}
		if err := deps.Auth.SendTaskReminder(ctx, item.email, name, item.message, formatReminderWhen(item.fireAt, item.timezone), route); err != nil {
			if !pushed {
				_, _ = deps.DB.ExecContext(ctx, `UPDATE reminder_occurrences SET claimed_until=NULL WHERE id=$1`, occurrenceID)
				return false, err
			}
			log.Warn().Err(err).Int64("occurrence", occurrenceID).Msg("reminder email failed; push delivered")
		} else {
			delivered = true
		}
	}
	if !delivered {
		_, _ = deps.DB.ExecContext(ctx, `UPDATE reminder_occurrences SET claimed_until=NULL WHERE id=$1`, occurrenceID)
		return false, errors.New("no reminder delivery channel succeeded")
	}
	if err := reminder.Complete(ctx, deps.DB, deps.ReminderQueue, occurrenceID); err != nil {
		return false, err
	}
	return true, nil
}
