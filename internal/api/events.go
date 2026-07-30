package api

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sajni/internal/db"
)

const maxEventVariables = 6

type eventVariable struct {
	ID        int64  `json:"id"`
	EventID   int64  `json:"event_id"`
	Name      string `json:"name"`
	Unit      string `json:"unit"`
	SortOrder int    `json:"sort_order"`
}

type eventValue struct {
	VariableID int64   `json:"variable_id"`
	Name       string  `json:"name"`
	Unit       string  `json:"unit"`
	Value      float64 `json:"value"`
}

type eventRecord struct {
	ID             int64           `json:"id"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Color          string          `json:"color"`
	Icon           string          `json:"icon"`
	Archived       bool            `json:"archived"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	LastOccurredAt *time.Time      `json:"last_occurred_at"`
	TotalEntries   int             `json:"total_entries"`
	Variables      []eventVariable `json:"variables"`
	LastValues     []eventValue    `json:"last_values"`
}

type eventEntry struct {
	ID         int64        `json:"id"`
	EventID    int64        `json:"event_id"`
	OccurredAt time.Time    `json:"occurred_at"`
	Note       string       `json:"note"`
	Values     []eventValue `json:"values"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
}

type eventValueInput struct {
	VariableID int64   `json:"variable_id"`
	Value      float64 `json:"value"`
}

func registerEventRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/events", listEvents(deps))
	mux.HandleFunc("POST /api/events", createEvent(deps))
	mux.HandleFunc("GET /api/events/entries", eventEntriesForDay(deps))
	mux.HandleFunc("GET /api/events/{id}", getEvent(deps))
	mux.HandleFunc("PUT /api/events/{id}", updateEvent(deps))
	mux.HandleFunc("DELETE /api/events/{id}", deleteEvent(deps))
	mux.HandleFunc("POST /api/events/{id}/variables", createEventVariable(deps))
	mux.HandleFunc("PUT /api/events/{id}/variables/{variableID}", updateEventVariable(deps))
	mux.HandleFunc("DELETE /api/events/{id}/variables/{variableID}", deleteEventVariable(deps))
	mux.HandleFunc("GET /api/events/{id}/entries", listEventEntries(deps))
	mux.HandleFunc("POST /api/events/{id}/entries", createEventEntry(deps))
	mux.HandleFunc("PUT /api/events/{id}/entries/{entryID}", updateEventEntry(deps))
	mux.HandleFunc("DELETE /api/events/{id}/entries/{entryID}", deleteEventEntry(deps))
	mux.HandleFunc("GET /api/events/{id}/trends", eventTrends(deps))
}

func listEvents(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		archived := queryParam(r, "archived") == "true"
		search := strings.TrimSpace(queryParam(r, "search"))
		like := "%" + search + "%"

		rows, err := d.QueryContext(r.Context(), `
			SELECT e.id, e.name, e.description, e.color, e.icon, e.archived,
			       e.created_at, e.updated_at,
			       (SELECT MAX(occurred_at) FROM event_entries ee
			        WHERE ee.user_id=e.user_id AND ee.event_id=e.id),
			       (SELECT COUNT(*) FROM event_entries ee
			        WHERE ee.user_id=e.user_id AND ee.event_id=e.id)
			FROM events e
			WHERE e.user_id=$1 AND e.archived=$2
			  AND ($3='' OR e.name ILIKE $4 OR e.description ILIKE $4)
			ORDER BY e.updated_at DESC`, uid, archived, search, like)
		if err != nil {
			internalError(w, r, "list events", err)
			return
		}
		defer rows.Close()

		events := []eventRecord{}
		for rows.Next() {
			var event eventRecord
			var last sql.NullTime
			if err := rows.Scan(
				&event.ID, &event.Name, &event.Description, &event.Color, &event.Icon,
				&event.Archived, &event.CreatedAt, &event.UpdatedAt, &last, &event.TotalEntries,
			); err != nil {
				internalError(w, r, "scan event", err)
				return
			}
			if last.Valid {
				event.LastOccurredAt = &last.Time
			}
			event.Variables, err = loadEventVariables(r.Context(), d, uid, event.ID)
			if err != nil {
				internalError(w, r, "load event variables", err)
				return
			}
			event.LastValues = []eventValue{}
			if last.Valid {
				var entryID int64
				if err := d.QueryRowContext(r.Context(), `
					SELECT id FROM event_entries
					WHERE user_id=$1 AND event_id=$2
					ORDER BY occurred_at DESC, id DESC LIMIT 1`, uid, event.ID).Scan(&entryID); err == nil {
					event.LastValues, err = loadEventValues(r.Context(), d, uid, event.ID, entryID)
					if err != nil {
						internalError(w, r, "load latest event values", err)
						return
					}
				}
			}
			events = append(events, event)
		}
		if err := rows.Err(); err != nil {
			internalError(w, r, "iterate events", err)
			return
		}
		writeJSON(w, http.StatusOK, events)
	}
}

func getEvent(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		event, err := loadEvent(r.Context(), d, uid, id)
		if err == sql.ErrNoRows {
			errJSON(w, http.StatusNotFound, "event not found")
			return
		}
		if err != nil {
			internalError(w, r, "get event", err)
			return
		}
		writeJSON(w, http.StatusOK, event)
	}
}

func createEvent(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Color       string `json:"color"`
			Icon        string `json:"icon"`
			Variables   []struct {
				Name string `json:"name"`
				Unit string `json:"unit"`
			} `json:"variables"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Description = strings.TrimSpace(body.Description)
		body.Color = strings.TrimSpace(body.Color)
		body.Icon = strings.TrimSpace(body.Icon)
		if body.Name == "" {
			errJSON(w, http.StatusBadRequest, "name is required")
			return
		}
		if len(body.Variables) > maxEventVariables {
			errJSON(w, http.StatusBadRequest, "an event can have at most 6 variables")
			return
		}
		if body.Color == "" {
			body.Color = "#2D5A4F"
		}
		if body.Icon == "" {
			body.Icon = "calendar"
		}

		tx, err := d.BeginTx(r.Context(), nil)
		if err != nil {
			internalError(w, r, "begin create event", err)
			return
		}
		defer tx.Rollback()

		var id int64
		err = tx.QueryRowContext(r.Context(), `
			INSERT INTO events (user_id, name, description, color, icon)
			VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			uid, body.Name, body.Description, body.Color, body.Icon,
		).Scan(&id)
		if err != nil {
			internalError(w, r, "insert event", err)
			return
		}
		for index, variable := range body.Variables {
			name := strings.TrimSpace(variable.Name)
			if name == "" {
				errJSON(w, http.StatusBadRequest, "variable name is required")
				return
			}
			if _, err := tx.ExecContext(r.Context(), `
				INSERT INTO event_variables (user_id, event_id, name, unit, sort_order)
				VALUES ($1,$2,$3,$4,$5)`,
				uid, id, name, strings.TrimSpace(variable.Unit), index,
			); err != nil {
				internalError(w, r, "insert event variable", err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit create event", err)
			return
		}

		event, err := loadEvent(r.Context(), d, uid, id)
		if err != nil {
			internalError(w, r, "load created event", err)
			return
		}
		writeJSON(w, http.StatusCreated, event)
	}
}

func updateEvent(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		var body struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			Color       *string `json:"color"`
			Icon        *string `json:"icon"`
			Archived    *bool   `json:"archived"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}

		sets := []string{}
		args := []any{}
		if body.Name != nil {
			name := strings.TrimSpace(*body.Name)
			if name == "" {
				errJSON(w, http.StatusBadRequest, "name is required")
				return
			}
			args = append(args, name)
			sets = append(sets, "name=$"+strconv.Itoa(len(args)))
		}
		if body.Description != nil {
			args = append(args, strings.TrimSpace(*body.Description))
			sets = append(sets, "description=$"+strconv.Itoa(len(args)))
		}
		if body.Color != nil {
			args = append(args, strings.TrimSpace(*body.Color))
			sets = append(sets, "color=$"+strconv.Itoa(len(args)))
		}
		if body.Icon != nil {
			icon := strings.TrimSpace(*body.Icon)
			if icon == "" {
				icon = "calendar"
			}
			args = append(args, icon)
			sets = append(sets, "icon=$"+strconv.Itoa(len(args)))
		}
		if body.Archived != nil {
			args = append(args, *body.Archived)
			sets = append(sets, "archived=$"+strconv.Itoa(len(args)))
		}
		if len(sets) == 0 {
			errJSON(w, http.StatusBadRequest, "nothing to update")
			return
		}

		args = append(args, id, uid)
		query := fmt.Sprintf(`
			UPDATE events SET %s, updated_at=NOW()
			WHERE id=$%d AND user_id=$%d`,
			strings.Join(sets, ", "), len(args)-1, len(args),
		)
		result, err := d.ExecContext(r.Context(), query, args...)
		if err != nil {
			internalError(w, r, "update event", err)
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			errJSON(w, http.StatusNotFound, "event not found")
			return
		}

		event, err := loadEvent(r.Context(), d, uid, id)
		if err != nil {
			internalError(w, r, "load updated event", err)
			return
		}
		writeJSON(w, http.StatusOK, event)
	}
}

func deleteEvent(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		result, err := d.ExecContext(r.Context(), "DELETE FROM events WHERE id=$1 AND user_id=$2", id, uid)
		if err != nil {
			internalError(w, r, "delete event", err)
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			errJSON(w, http.StatusNotFound, "event not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func createEventVariable(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		eventID, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid event id")
			return
		}
		var body struct {
			Name string `json:"name"`
			Unit string `json:"unit"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Unit = strings.TrimSpace(body.Unit)
		if body.Name == "" {
			errJSON(w, http.StatusBadRequest, "variable name is required")
			return
		}
		var count int
		if err := d.QueryRowContext(r.Context(), `
			SELECT COUNT(*) FROM event_variables v
			JOIN events e ON e.id=v.event_id
			WHERE v.user_id=$1 AND v.event_id=$2 AND e.user_id=$1`,
			uid, eventID,
		).Scan(&count); err != nil {
			internalError(w, r, "count event variables", err)
			return
		}
		if count >= maxEventVariables {
			errJSON(w, http.StatusBadRequest, "an event can have at most 6 variables")
			return
		}

		var variable eventVariable
		err = d.QueryRowContext(r.Context(), `
			INSERT INTO event_variables (user_id, event_id, name, unit, sort_order)
			SELECT $1,$2,$3,$4,$5 FROM events
			WHERE id=$2 AND user_id=$1
			RETURNING id, event_id, name, unit, sort_order`,
			uid, eventID, body.Name, body.Unit, count,
		).Scan(&variable.ID, &variable.EventID, &variable.Name, &variable.Unit, &variable.SortOrder)
		if err == sql.ErrNoRows {
			errJSON(w, http.StatusNotFound, "event not found")
			return
		}
		if err != nil {
			internalError(w, r, "create event variable", err)
			return
		}
		writeJSON(w, http.StatusCreated, variable)
	}
}

func updateEventVariable(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		eventID, variableID, ok := eventVariableParams(w, r)
		if !ok {
			return
		}
		var body struct {
			Name      *string `json:"name"`
			Unit      *string `json:"unit"`
			SortOrder *int    `json:"sort_order"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		if body.Name != nil {
			name := strings.TrimSpace(*body.Name)
			if name == "" {
				errJSON(w, http.StatusBadRequest, "variable name is required")
				return
			}
			if _, err := d.ExecContext(r.Context(), `
				UPDATE event_variables SET name=$1
				WHERE id=$2 AND event_id=$3 AND user_id=$4`,
				name, variableID, eventID, uid,
			); err != nil {
				internalError(w, r, "update variable name", err)
				return
			}
		}
		if body.Unit != nil {
			if _, err := d.ExecContext(r.Context(), `
				UPDATE event_variables SET unit=$1
				WHERE id=$2 AND event_id=$3 AND user_id=$4`,
				strings.TrimSpace(*body.Unit), variableID, eventID, uid,
			); err != nil {
				internalError(w, r, "update variable unit", err)
				return
			}
		}
		if body.SortOrder != nil {
			if _, err := d.ExecContext(r.Context(), `
				UPDATE event_variables SET sort_order=$1
				WHERE id=$2 AND event_id=$3 AND user_id=$4`,
				*body.SortOrder, variableID, eventID, uid,
			); err != nil {
				internalError(w, r, "update variable order", err)
				return
			}
		}

		var variable eventVariable
		err := d.QueryRowContext(r.Context(), `
			SELECT id, event_id, name, unit, sort_order
			FROM event_variables WHERE id=$1 AND event_id=$2 AND user_id=$3`,
			variableID, eventID, uid,
		).Scan(&variable.ID, &variable.EventID, &variable.Name, &variable.Unit, &variable.SortOrder)
		if err == sql.ErrNoRows {
			errJSON(w, http.StatusNotFound, "variable not found")
			return
		}
		if err != nil {
			internalError(w, r, "load updated variable", err)
			return
		}
		writeJSON(w, http.StatusOK, variable)
	}
}

func deleteEventVariable(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		eventID, variableID, ok := eventVariableParams(w, r)
		if !ok {
			return
		}
		result, err := d.ExecContext(r.Context(), `
			DELETE FROM event_variables
			WHERE id=$1 AND event_id=$2 AND user_id=$3`,
			variableID, eventID, uid,
		)
		if err != nil {
			internalError(w, r, "delete event variable", err)
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			errJSON(w, http.StatusNotFound, "variable not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func listEventEntries(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		eventID, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid event id")
			return
		}
		var owned bool
		if err := d.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM events WHERE id=$1 AND user_id=$2)", eventID, uid).Scan(&owned); err != nil {
			internalError(w, r, "check event ownership", err)
			return
		}
		if !owned {
			errJSON(w, http.StatusNotFound, "event not found")
			return
		}

		clauses := []string{"user_id=$1", "event_id=$2"}
		args := []any{uid, eventID}
		search := strings.TrimSpace(queryParam(r, "search"))
		if search != "" {
			args = append(args, "%"+search+"%")
			clauses = append(clauses, "note ILIKE $"+strconv.Itoa(len(args)))
		}
		if from := strings.TrimSpace(queryParam(r, "from")); from != "" {
			start, err := parseEventDate(from, userLocation(d, uid))
			if err != nil {
				errJSON(w, http.StatusBadRequest, "invalid from date")
				return
			}
			args = append(args, start)
			clauses = append(clauses, "occurred_at >= $"+strconv.Itoa(len(args)))
		}
		if to := strings.TrimSpace(queryParam(r, "to")); to != "" {
			end, err := parseEventDate(to, userLocation(d, uid))
			if err != nil {
				errJSON(w, http.StatusBadRequest, "invalid to date")
				return
			}
			args = append(args, end.AddDate(0, 0, 1))
			clauses = append(clauses, "occurred_at < $"+strconv.Itoa(len(args)))
		}
		if before := strings.TrimSpace(queryParam(r, "before")); before != "" {
			cursor, err := time.Parse(time.RFC3339, before)
			if err != nil {
				errJSON(w, http.StatusBadRequest, "invalid cursor")
				return
			}
			args = append(args, cursor)
			clauses = append(clauses, "occurred_at < $"+strconv.Itoa(len(args)))
		}
		limit := 50
		if parsed, err := strconv.Atoi(queryParam(r, "limit")); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
		args = append(args, limit+1)
		query := `
			SELECT id, event_id, occurred_at, note, created_at, updated_at
			FROM event_entries WHERE ` + strings.Join(clauses, " AND ") + `
			ORDER BY occurred_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))
		rows, err := d.QueryContext(r.Context(), query, args...)
		if err != nil {
			internalError(w, r, "list event entries", err)
			return
		}
		defer rows.Close()

		entries := []eventEntry{}
		for rows.Next() {
			var entry eventEntry
			if err := rows.Scan(
				&entry.ID, &entry.EventID, &entry.OccurredAt, &entry.Note,
				&entry.CreatedAt, &entry.UpdatedAt,
			); err != nil {
				internalError(w, r, "scan event entry", err)
				return
			}
			entry.Values, err = loadEventValues(r.Context(), d, uid, eventID, entry.ID)
			if err != nil {
				internalError(w, r, "load event entry values", err)
				return
			}
			entries = append(entries, entry)
		}
		if err := rows.Err(); err != nil {
			internalError(w, r, "iterate event entries", err)
			return
		}

		nextCursor := ""
		if len(entries) > limit {
			nextCursor = entries[limit-1].OccurredAt.Format(time.RFC3339Nano)
			entries = entries[:limit]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"entries":     entries,
			"next_cursor": nextCursor,
		})
	}
}

func createEventEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		eventID, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid event id")
			return
		}
		var body struct {
			OccurredAt string            `json:"occurred_at"`
			Note       string            `json:"note"`
			Values     []eventValueInput `json:"values"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		occurredAt, err := time.Parse(time.RFC3339, strings.TrimSpace(body.OccurredAt))
		if err != nil {
			errJSON(w, http.StatusBadRequest, "occurred_at must be an RFC3339 timestamp")
			return
		}

		tx, err := d.BeginTx(r.Context(), nil)
		if err != nil {
			internalError(w, r, "begin create event entry", err)
			return
		}
		defer tx.Rollback()

		var entryID int64
		err = tx.QueryRowContext(r.Context(), `
			INSERT INTO event_entries (user_id, event_id, occurred_at, note)
			SELECT $1,$2,$3,$4 FROM events WHERE id=$2 AND user_id=$1
			RETURNING id`,
			uid, eventID, occurredAt, strings.TrimSpace(body.Note),
		).Scan(&entryID)
		if err == sql.ErrNoRows {
			errJSON(w, http.StatusNotFound, "event not found")
			return
		}
		if err != nil {
			internalError(w, r, "insert event entry", err)
			return
		}
		if err := insertEventValues(r.Context(), tx, uid, eventID, entryID, body.Values); err != nil {
			errJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := tx.ExecContext(r.Context(), "UPDATE events SET updated_at=NOW() WHERE id=$1 AND user_id=$2", eventID, uid); err != nil {
			internalError(w, r, "touch event", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit event entry", err)
			return
		}

		entry, err := loadEventEntry(r.Context(), d, uid, eventID, entryID)
		if err != nil {
			internalError(w, r, "load created event entry", err)
			return
		}
		writeJSON(w, http.StatusCreated, entry)
	}
}

func updateEventEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		eventID, entryID, ok := eventEntryParams(w, r)
		if !ok {
			return
		}
		var body struct {
			OccurredAt *string            `json:"occurred_at"`
			Note       *string            `json:"note"`
			Values     *[]eventValueInput `json:"values"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}

		tx, err := d.BeginTx(r.Context(), nil)
		if err != nil {
			internalError(w, r, "begin update event entry", err)
			return
		}
		defer tx.Rollback()
		var owned bool
		if err := tx.QueryRowContext(r.Context(), `
			SELECT EXISTS(
				SELECT 1 FROM event_entries
				WHERE id=$1 AND event_id=$2 AND user_id=$3
			)`, entryID, eventID, uid).Scan(&owned); err != nil {
			internalError(w, r, "check event entry ownership", err)
			return
		}
		if !owned {
			errJSON(w, http.StatusNotFound, "entry not found")
			return
		}

		if body.OccurredAt != nil {
			occurredAt, err := time.Parse(time.RFC3339, strings.TrimSpace(*body.OccurredAt))
			if err != nil {
				errJSON(w, http.StatusBadRequest, "occurred_at must be an RFC3339 timestamp")
				return
			}
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE event_entries SET occurred_at=$1, updated_at=NOW()
				WHERE id=$2 AND event_id=$3 AND user_id=$4`,
				occurredAt, entryID, eventID, uid,
			); err != nil {
				internalError(w, r, "update event entry time", err)
				return
			}
		}
		if body.Note != nil {
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE event_entries SET note=$1, updated_at=NOW()
				WHERE id=$2 AND event_id=$3 AND user_id=$4`,
				strings.TrimSpace(*body.Note), entryID, eventID, uid,
			); err != nil {
				internalError(w, r, "update event entry note", err)
				return
			}
		}
		if body.Values != nil {
			if _, err := tx.ExecContext(r.Context(), "DELETE FROM event_entry_values WHERE entry_id=$1", entryID); err != nil {
				internalError(w, r, "clear event entry values", err)
				return
			}
			if err := insertEventValues(r.Context(), tx, uid, eventID, entryID, *body.Values); err != nil {
				errJSON(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if _, err := tx.ExecContext(r.Context(), "UPDATE events SET updated_at=NOW() WHERE id=$1 AND user_id=$2", eventID, uid); err != nil {
			internalError(w, r, "touch event", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit event entry update", err)
			return
		}

		entry, err := loadEventEntry(r.Context(), d, uid, eventID, entryID)
		if err == sql.ErrNoRows {
			errJSON(w, http.StatusNotFound, "entry not found")
			return
		}
		if err != nil {
			internalError(w, r, "load updated event entry", err)
			return
		}
		writeJSON(w, http.StatusOK, entry)
	}
}

func deleteEventEntry(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		eventID, entryID, ok := eventEntryParams(w, r)
		if !ok {
			return
		}
		result, err := d.ExecContext(r.Context(), `
			DELETE FROM event_entries
			WHERE id=$1 AND event_id=$2 AND user_id=$3`,
			entryID, eventID, uid,
		)
		if err != nil {
			internalError(w, r, "delete event entry", err)
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			errJSON(w, http.StatusNotFound, "entry not found")
			return
		}
		_, _ = d.ExecContext(r.Context(), "UPDATE events SET updated_at=NOW() WHERE id=$1 AND user_id=$2", eventID, uid)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func eventEntriesForDay(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		date := strings.TrimSpace(queryParam(r, "date"))
		start, err := parseEventDate(date, userLocation(d, uid))
		if err != nil {
			errJSON(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
			return
		}
		rows, err := d.QueryContext(r.Context(), `
			SELECT ee.id, ee.event_id, e.name, e.icon, e.color, ee.occurred_at, ee.note
			FROM event_entries ee
			JOIN events e ON e.id=ee.event_id AND e.user_id=ee.user_id
			WHERE ee.user_id=$1 AND ee.occurred_at >= $2 AND ee.occurred_at < $3
			ORDER BY ee.occurred_at ASC, ee.id ASC`,
			uid, start, start.AddDate(0, 0, 1),
		)
		if err != nil {
			internalError(w, r, "list event entries for day", err)
			return
		}
		defer rows.Close()

		type dayEntry struct {
			ID         int64     `json:"id"`
			EventID    int64     `json:"event_id"`
			EventName  string    `json:"event_name"`
			Icon       string    `json:"icon"`
			Color      string    `json:"color"`
			OccurredAt time.Time `json:"occurred_at"`
			Note       string    `json:"note"`
		}
		entries := []dayEntry{}
		for rows.Next() {
			var entry dayEntry
			if err := rows.Scan(
				&entry.ID, &entry.EventID, &entry.EventName, &entry.Icon,
				&entry.Color, &entry.OccurredAt, &entry.Note,
			); err != nil {
				internalError(w, r, "scan event day entry", err)
				return
			}
			entries = append(entries, entry)
		}
		if err := rows.Err(); err != nil {
			internalError(w, r, "iterate event day entries", err)
			return
		}
		writeJSON(w, http.StatusOK, entries)
	}
}

func eventTrends(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		eventID, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid event id")
			return
		}
		var owned bool
		if err := d.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM events WHERE id=$1 AND user_id=$2)", eventID, uid).Scan(&owned); err != nil {
			internalError(w, r, "check trend event ownership", err)
			return
		}
		if !owned {
			errJSON(w, http.StatusNotFound, "event not found")
			return
		}

		rows, err := d.QueryContext(r.Context(), `
			SELECT id, occurred_at FROM event_entries
			WHERE user_id=$1 AND event_id=$2
			ORDER BY occurred_at ASC, id ASC`, uid, eventID)
		if err != nil {
			internalError(w, r, "load event trend entries", err)
			return
		}
		defer rows.Close()

		type trendPoint struct {
			EntryID    int64              `json:"entry_id"`
			OccurredAt time.Time          `json:"occurred_at"`
			GapDays    *float64           `json:"gap_days"`
			Values     map[string]float64 `json:"values"`
		}
		points := []trendPoint{}
		entryIndex := map[int64]int{}
		var previous time.Time
		gapTotal := 0.0
		gapCount := 0
		for rows.Next() {
			var point trendPoint
			if err := rows.Scan(&point.EntryID, &point.OccurredAt); err != nil {
				internalError(w, r, "scan event trend entry", err)
				return
			}
			point.Values = map[string]float64{}
			if !previous.IsZero() {
				days := point.OccurredAt.Sub(previous).Hours() / 24
				days = math.Round(days*100) / 100
				point.GapDays = &days
				gapTotal += days
				gapCount++
			}
			previous = point.OccurredAt
			entryIndex[point.EntryID] = len(points)
			points = append(points, point)
		}
		if err := rows.Err(); err != nil {
			internalError(w, r, "iterate event trends", err)
			return
		}

		valueRows, err := d.QueryContext(r.Context(), `
			SELECT ev.entry_id, ev.variable_id, ev.value
			FROM event_entry_values ev
			JOIN event_entries ee ON ee.id=ev.entry_id
			WHERE ee.user_id=$1 AND ee.event_id=$2`, uid, eventID)
		if err != nil {
			internalError(w, r, "load event trend values", err)
			return
		}
		for valueRows.Next() {
			var entryID, variableID int64
			var value float64
			if err := valueRows.Scan(&entryID, &variableID, &value); err != nil {
				valueRows.Close()
				internalError(w, r, "scan event trend value", err)
				return
			}
			if index, ok := entryIndex[entryID]; ok {
				points[index].Values[strconv.FormatInt(variableID, 10)] = value
			}
		}
		if err := valueRows.Err(); err != nil {
			valueRows.Close()
			internalError(w, r, "iterate event trend values", err)
			return
		}
		valueRows.Close()

		var averageGap *float64
		if gapCount > 0 {
			average := math.Round((gapTotal/float64(gapCount))*100) / 100
			averageGap = &average
		}
		var lastOccurredAt *time.Time
		if len(points) > 0 {
			last := points[len(points)-1].OccurredAt
			lastOccurredAt = &last
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total_entries":    len(points),
			"last_occurred_at": lastOccurredAt,
			"average_gap_days": averageGap,
			"points":           points,
		})
	}
}

func loadEvent(ctx context.Context, d *db.DB, uid string, id int64) (eventRecord, error) {
	var event eventRecord
	var last sql.NullTime
	err := d.QueryRowContext(ctx, `
		SELECT e.id, e.name, e.description, e.color, e.icon, e.archived,
		       e.created_at, e.updated_at,
		       (SELECT MAX(occurred_at) FROM event_entries ee
		        WHERE ee.user_id=e.user_id AND ee.event_id=e.id),
		       (SELECT COUNT(*) FROM event_entries ee
		        WHERE ee.user_id=e.user_id AND ee.event_id=e.id)
		FROM events e WHERE e.id=$1 AND e.user_id=$2`, id, uid,
	).Scan(
		&event.ID, &event.Name, &event.Description, &event.Color, &event.Icon,
		&event.Archived, &event.CreatedAt, &event.UpdatedAt, &last, &event.TotalEntries,
	)
	if err != nil {
		return event, err
	}
	if last.Valid {
		event.LastOccurredAt = &last.Time
	}
	event.Variables, err = loadEventVariables(ctx, d, uid, id)
	if err != nil {
		return event, fmt.Errorf("load variables: %w", err)
	}
	event.LastValues = []eventValue{}
	if last.Valid {
		var entryID int64
		if err := d.QueryRowContext(ctx, `
			SELECT id FROM event_entries
			WHERE user_id=$1 AND event_id=$2
			ORDER BY occurred_at DESC, id DESC LIMIT 1`, uid, id).Scan(&entryID); err == nil {
			event.LastValues, err = loadEventValues(ctx, d, uid, id, entryID)
			if err != nil {
				return event, fmt.Errorf("load latest values: %w", err)
			}
		}
	}
	return event, nil
}

func loadEventVariables(ctx context.Context, d *db.DB, uid string, eventID int64) ([]eventVariable, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, event_id, name, unit, sort_order
		FROM event_variables
		WHERE user_id=$1 AND event_id=$2
		ORDER BY sort_order ASC, id ASC`, uid, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	variables := []eventVariable{}
	for rows.Next() {
		var variable eventVariable
		if err := rows.Scan(
			&variable.ID, &variable.EventID, &variable.Name,
			&variable.Unit, &variable.SortOrder,
		); err != nil {
			return nil, err
		}
		variables = append(variables, variable)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return variables, nil
}

func loadEventValues(ctx context.Context, d *db.DB, uid string, eventID, entryID int64) ([]eventValue, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT v.id, v.name, v.unit, ev.value
		FROM event_entry_values ev
		JOIN event_variables v ON v.id=ev.variable_id
		JOIN event_entries ee ON ee.id=ev.entry_id
		WHERE ee.id=$1 AND ee.event_id=$2 AND ee.user_id=$3
		ORDER BY v.sort_order ASC, v.id ASC`, entryID, eventID, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := []eventValue{}
	for rows.Next() {
		var value eventValue
		if err := rows.Scan(&value.VariableID, &value.Name, &value.Unit, &value.Value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func loadEventEntry(ctx context.Context, d *db.DB, uid string, eventID, entryID int64) (eventEntry, error) {
	var entry eventEntry
	err := d.QueryRowContext(ctx, `
		SELECT id, event_id, occurred_at, note, created_at, updated_at
		FROM event_entries
		WHERE id=$1 AND event_id=$2 AND user_id=$3`,
		entryID, eventID, uid,
	).Scan(
		&entry.ID, &entry.EventID, &entry.OccurredAt, &entry.Note,
		&entry.CreatedAt, &entry.UpdatedAt,
	)
	if err != nil {
		return entry, err
	}
	entry.Values, err = loadEventValues(ctx, d, uid, eventID, entryID)
	if err != nil {
		return entry, err
	}
	return entry, nil
}

func insertEventValues(
	ctx context.Context,
	tx *sql.Tx,
	uid string,
	eventID int64,
	entryID int64,
	values []eventValueInput,
) error {
	seen := map[int64]bool{}
	for _, value := range values {
		if value.VariableID == 0 {
			return fmt.Errorf("variable_id is required")
		}
		if seen[value.VariableID] {
			return fmt.Errorf("variable %d appears more than once", value.VariableID)
		}
		seen[value.VariableID] = true
		result, err := tx.ExecContext(ctx, `
			INSERT INTO event_entry_values (entry_id, variable_id, value)
			SELECT $1, v.id, $2 FROM event_variables v
			WHERE v.id=$3 AND v.event_id=$4 AND v.user_id=$5`,
			entryID, value.Value, value.VariableID, eventID, uid,
		)
		if err != nil {
			return fmt.Errorf("insert variable %d: %w", value.VariableID, err)
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			return fmt.Errorf("variable %d does not belong to this event", value.VariableID)
		}
	}
	return nil
}

func eventVariableParams(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	eventID, err := intParam(r, "id")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid event id")
		return 0, 0, false
	}
	variableID, err := intParam(r, "variableID")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid variable id")
		return 0, 0, false
	}
	return eventID, variableID, true
}

func eventEntryParams(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	eventID, err := intParam(r, "id")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid event id")
		return 0, 0, false
	}
	entryID, err := intParam(r, "entryID")
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid entry id")
		return 0, 0, false
	}
	return eventID, entryID, true
}

func parseEventDate(value string, location *time.Location) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("date is required")
	}
	return time.ParseInLocation("2006-01-02", value, location)
}
