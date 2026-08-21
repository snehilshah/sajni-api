package reminder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sajni/internal/db"
	"sajni/internal/reminderqueue"
)

var (
	ErrInvalid  = errors.New("invalid reminder")
	ErrNotFound = errors.New("reminder not found")
)

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

type Occurrence struct {
	ID          int64      `json:"id"`
	Sequence    int        `json:"sequence"`
	ScheduledAt time.Time  `json:"scheduled_at"`
	FireAt      time.Time  `json:"fire_at"`
	Status      string     `json:"status"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	SkippedAt   *time.Time `json:"skipped_at,omitempty"`
}

type Reminder struct {
	ID             int64       `json:"id"`
	Message        string      `json:"message"`
	Notes          string      `json:"notes"`
	Timezone       string      `json:"timezone"`
	StartsAt       time.Time   `json:"starts_at"`
	Recurrence     Rule        `json:"recurrence"`
	Active         bool        `json:"active"`
	NextOccurrence *Occurrence `json:"next_occurrence,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

type HistoryItem struct {
	Occurrence
	ReminderID int64  `json:"reminder_id"`
	Message    string `json:"message"`
	Notes      string `json:"notes"`
}

type SaveInput struct {
	Message    string `json:"message"`
	Notes      string `json:"notes"`
	Timezone   string `json:"timezone"`
	StartsAt   string `json:"starts_at"`
	Recurrence Rule   `json:"recurrence"`
}

func UserTimezone(ctx context.Context, d *db.DB, uid string) string {
	var timezone string
	_ = d.QueryRowContext(ctx, `SELECT COALESCE(timezone,'') FROM users WHERE id=$1`, uid).Scan(&timezone)
	if _, err := time.LoadLocation(timezone); err != nil {
		return "Asia/Kolkata"
	}
	return timezone
}

func ParseInput(input SaveInput, fallbackTimezone string) (SaveInput, time.Time, error) {
	input.Message = strings.TrimSpace(input.Message)
	input.Notes = strings.TrimSpace(input.Notes)
	if input.Message == "" {
		return input, time.Time{}, invalid("message required")
	}
	if len(input.Message) > 500 || len(input.Notes) > 4000 {
		return input, time.Time{}, invalid("reminder content is too long")
	}
	if input.Timezone == "" {
		input.Timezone = fallbackTimezone
	}
	loc, err := time.LoadLocation(input.Timezone)
	if err != nil {
		return input, time.Time{}, invalid("invalid timezone")
	}
	startsAt, err := time.Parse(time.RFC3339, input.StartsAt)
	if err != nil {
		return input, time.Time{}, invalid("starts_at must be an ISO timestamp with offset")
	}
	if startsAt.Before(time.Now().Add(-time.Minute)) {
		return input, time.Time{}, invalid("reminder time must be in the future")
	}
	input.Recurrence, err = Normalize(input.Recurrence, startsAt.In(loc))
	if err != nil {
		return input, time.Time{}, invalid(err.Error())
	}
	return input, startsAt, nil
}

func Create(ctx context.Context, d *db.DB, queue reminderqueue.Queue, uid string, input SaveInput) (Reminder, error) {
	input, startsAt, err := ParseInput(input, UserTimezone(ctx, d, uid))
	if err != nil {
		return Reminder{}, err
	}
	recurrenceJSON, _ := json.Marshal(input.Recurrence)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Reminder{}, err
	}
	defer tx.Rollback()
	var id, occurrenceID int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO reminders (user_id,message,notes,timezone,starts_at,recurrence)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		uid, input.Message, input.Notes, input.Timezone, startsAt, recurrenceJSON,
	).Scan(&id); err != nil {
		return Reminder{}, err
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO reminder_occurrences (reminder_id,user_id,sequence,scheduled_at,fire_at)
		VALUES ($1,$2,1,$3,$3) RETURNING id`, id, uid, startsAt,
	).Scan(&occurrenceID); err != nil {
		return Reminder{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reminder{}, err
	}
	_ = queue.EnqueueStandalone(ctx, occurrenceID, startsAt)
	return Get(ctx, d, uid, id)
}

// Replace edits an entire series. Delivered/skipped history stays intact; the
// one pending occurrence is cancelled and replaced from the new anchor.
func Replace(ctx context.Context, d *db.DB, queue reminderqueue.Queue, uid string, id int64, input SaveInput) (Reminder, error) {
	input, startsAt, err := ParseInput(input, UserTimezone(ctx, d, uid))
	if err != nil {
		return Reminder{}, err
	}
	recurrenceJSON, _ := json.Marshal(input.Recurrence)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Reminder{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE reminders SET message=$1,notes=$2,timezone=$3,starts_at=$4,recurrence=$5,active=TRUE,updated_at=NOW() WHERE id=$6 AND user_id=$7`,
		input.Message, input.Notes, input.Timezone, startsAt, recurrenceJSON, id, uid)
	if err != nil {
		return Reminder{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Reminder{}, ErrNotFound
	}
	var sequence int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(
			MAX(sequence) FILTER (WHERE status='pending'),
			MAX(sequence)+1,
			1
		) FROM reminder_occurrences WHERE reminder_id=$1 AND user_id=$2`, id, uid).Scan(&sequence); err != nil {
		return Reminder{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE reminder_occurrences SET status='cancelled',claimed_until=NULL WHERE reminder_id=$1 AND user_id=$2 AND status='pending'`, id, uid); err != nil {
		return Reminder{}, err
	}
	var occurrenceID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO reminder_occurrences (reminder_id,user_id,sequence,scheduled_at,fire_at) VALUES ($1,$2,$3,$4,$4) RETURNING id`, id, uid, sequence, startsAt).Scan(&occurrenceID); err != nil {
		return Reminder{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reminder{}, err
	}
	_ = queue.EnqueueStandalone(ctx, occurrenceID, startsAt)
	return Get(ctx, d, uid, id)
}

func Get(ctx context.Context, d *db.DB, uid string, id int64) (Reminder, error) {
	var item Reminder
	var recurrenceJSON []byte
	err := d.QueryRowContext(ctx, `SELECT id,message,notes,timezone,starts_at,recurrence,active,created_at,updated_at FROM reminders WHERE id=$1 AND user_id=$2`, id, uid).
		Scan(&item.ID, &item.Message, &item.Notes, &item.Timezone, &item.StartsAt, &recurrenceJSON, &item.Active, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Reminder{}, ErrNotFound
	}
	if err != nil {
		return Reminder{}, err
	}
	_ = json.Unmarshal(recurrenceJSON, &item.Recurrence)
	var occurrence Occurrence
	err = d.QueryRowContext(ctx, `SELECT id,sequence,scheduled_at,fire_at,status,delivered_at,skipped_at FROM reminder_occurrences WHERE reminder_id=$1 AND user_id=$2 AND status='pending' ORDER BY fire_at LIMIT 1`, id, uid).
		Scan(&occurrence.ID, &occurrence.Sequence, &occurrence.ScheduledAt, &occurrence.FireAt, &occurrence.Status, &occurrence.DeliveredAt, &occurrence.SkippedAt)
	if err == nil {
		item.NextOccurrence = &occurrence
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Reminder{}, err
	}
	return item, nil
}

func List(ctx context.Context, d *db.DB, uid, search string, activeOnly bool, limit int) ([]Reminder, error) {
	if limit <= 0 || limit > 250 {
		limit = 100
	}
	rows, err := d.QueryContext(ctx, `SELECT id FROM reminders WHERE user_id=$1 AND ($2=FALSE OR active=TRUE) AND ($3='' OR message ILIKE '%'||$3||'%' OR notes ILIKE '%'||$3||'%') ORDER BY active DESC, updated_at DESC LIMIT $4`, uid, activeOnly, strings.TrimSpace(search), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	items := make([]Reminder, 0, len(ids))
	for _, id := range ids {
		item, err := Get(ctx, d, uid, id)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func History(ctx context.Context, d *db.DB, uid string, limit, offset int) ([]HistoryItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.QueryContext(ctx, `
		SELECT o.id,o.sequence,o.scheduled_at,o.fire_at,o.status,o.delivered_at,o.skipped_at,
		       r.id,r.message,r.notes
		FROM reminder_occurrences o JOIN reminders r ON r.id=o.reminder_id
		WHERE o.user_id=$1 AND o.status IN ('delivered','skipped')
		ORDER BY COALESCE(o.delivered_at,o.skipped_at,o.fire_at) DESC LIMIT $2 OFFSET $3`, uid, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []HistoryItem{}
	for rows.Next() {
		var item HistoryItem
		if err := rows.Scan(&item.ID, &item.Sequence, &item.ScheduledAt, &item.FireAt, &item.Status, &item.DeliveredAt, &item.SkippedAt, &item.ReminderID, &item.Message, &item.Notes); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func Snooze(ctx context.Context, d *db.DB, queue reminderqueue.Queue, uid string, id, occurrenceID int64, fireAt time.Time) (Reminder, error) {
	if fireAt.Before(time.Now().Add(time.Minute)) {
		return Reminder{}, invalid("snooze time must be in the future")
	}
	var oid int64
	err := d.QueryRowContext(ctx, `UPDATE reminder_occurrences SET fire_at=$1,claimed_until=NULL WHERE reminder_id=$2 AND user_id=$3 AND status='pending' AND ($4=0 OR id=$4) RETURNING id`, fireAt, id, uid, occurrenceID).Scan(&oid)
	if errors.Is(err, sql.ErrNoRows) {
		return Reminder{}, ErrNotFound
	}
	if err != nil {
		return Reminder{}, err
	}
	_ = queue.EnqueueStandalone(ctx, oid, fireAt)
	return Get(ctx, d, uid, id)
}

func Skip(ctx context.Context, d *db.DB, queue reminderqueue.Queue, uid string, id, occurrenceID int64) (Reminder, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Reminder{}, err
	}
	defer tx.Rollback()
	var scheduled time.Time
	var sequence int
	err = tx.QueryRowContext(ctx, `UPDATE reminder_occurrences SET status='skipped',skipped_at=NOW(),claimed_until=NULL WHERE reminder_id=$1 AND user_id=$2 AND status='pending' AND ($3=0 OR id=$3) RETURNING scheduled_at,sequence`, id, uid, occurrenceID).Scan(&scheduled, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Reminder{}, ErrNotFound
	}
	if err != nil {
		return Reminder{}, err
	}
	nextID, nextAt, err := advanceTx(ctx, tx, id, uid, scheduled, sequence)
	if err != nil {
		return Reminder{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reminder{}, err
	}
	if nextID > 0 {
		_ = queue.EnqueueStandalone(ctx, nextID, nextAt)
	}
	return Get(ctx, d, uid, id)
}

func Delete(ctx context.Context, d *db.DB, uid string, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM reminders WHERE id=$1 AND user_id=$2`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Complete marks one claimed occurrence delivered and atomically creates the
// next series occurrence. It is safe to call after a stale Cloud Task: only a
// still-pending row can transition.
func Complete(ctx context.Context, d *db.DB, queue reminderqueue.Queue, occurrenceID int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var reminderID int64
	var uid string
	var scheduled time.Time
	var sequence int
	err = tx.QueryRowContext(ctx, `UPDATE reminder_occurrences SET status='delivered',delivered_at=NOW(),claimed_until=NULL WHERE id=$1 AND status='pending' RETURNING reminder_id,user_id,scheduled_at,sequence`, occurrenceID).
		Scan(&reminderID, &uid, &scheduled, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	nextID, nextAt, err := advanceTx(ctx, tx, reminderID, uid, scheduled, sequence)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if nextID > 0 {
		_ = queue.EnqueueStandalone(ctx, nextID, nextAt)
	}
	return nil
}

func advanceTx(ctx context.Context, tx *sql.Tx, reminderID int64, uid string, scheduled time.Time, sequence int) (int64, time.Time, error) {
	var startsAt time.Time
	var timezone string
	var recurrenceJSON []byte
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT starts_at,timezone,recurrence,active FROM reminders WHERE id=$1 AND user_id=$2 FOR UPDATE`, reminderID, uid).
		Scan(&startsAt, &timezone, &recurrenceJSON, &active); err != nil {
		return 0, time.Time{}, err
	}
	var rule Rule
	_ = json.Unmarshal(recurrenceJSON, &rule)
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	next, ok := Next(startsAt, scheduled, sequence, rule, loc)
	if !active || !ok {
		_, err := tx.ExecContext(ctx, `UPDATE reminders SET active=FALSE,updated_at=NOW() WHERE id=$1 AND user_id=$2`, reminderID, uid)
		return 0, time.Time{}, err
	}
	var nextID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO reminder_occurrences (reminder_id,user_id,sequence,scheduled_at,fire_at) VALUES ($1,$2,$3,$4,$4) RETURNING id`, reminderID, uid, sequence+1, next).Scan(&nextID); err != nil {
		return 0, time.Time{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE reminders SET updated_at=NOW() WHERE id=$1`, reminderID)
	return nextID, next, err
}
