package thinking

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"sajni/internal/db"
)

var (
	ErrNotFound        = errors.New("card not found")
	ErrNotActionable   = errors.New("only todo and contradiction cards can be closed")
	ErrCommentRequired = errors.New("a contradiction needs a resolution comment")
	ErrEmptyComment    = errors.New("comment is required")
	ErrCommentTooLong  = errors.New("comment is too long")
)

const MaxCommentLength = 4000

type Event struct {
	ID        int64     `json:"id"`
	CardID    int64     `json:"card_id"`
	Kind      string    `json:"kind"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func validateComment(body string, required bool) (string, error) {
	body = strings.TrimSpace(body)
	if required && body == "" {
		return "", ErrEmptyComment
	}
	if len([]rune(body)) > MaxCommentLength {
		return "", ErrCommentTooLong
	}
	return body, nil
}

func validateStateChange(kind string, closed bool, comment string) (string, error) {
	if kind != "todo" && kind != "contradiction" {
		return "", ErrNotActionable
	}
	comment, err := validateComment(comment, closed && kind == "contradiction")
	if errors.Is(err, ErrEmptyComment) {
		return "", ErrCommentRequired
	}
	return comment, err
}

// AddComment records user context without replacing the original card text.
func AddComment(ctx context.Context, d *db.DB, uid string, cardID int64, body string) error {
	body, err := validateComment(body, true)
	if err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM thinking_cards WHERE id=$1 AND user_id=$2`, cardID, uid).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO thinking_card_events (card_id,user_id,kind,body) VALUES ($1,$2,'comment',$3)`, cardID, uid, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE thinking_projects SET context_updated_at=NOW(),updated_at=NOW() WHERE id=$1 AND user_id=$2`, projectID, uid); err != nil {
		return err
	}
	return tx.Commit()
}

// SetClosed changes an actionable card's state and records the explanation
// in the same transaction. Repeating the current state creates no event.
func SetClosed(ctx context.Context, d *db.DB, uid string, cardID int64, closed bool, comment string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectID int64
	var kind, status string
	if err := tx.QueryRowContext(ctx, `SELECT project_id,kind,status FROM thinking_cards WHERE id=$1 AND user_id=$2 FOR UPDATE`, cardID, uid).Scan(&projectID, &kind, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if kind != "todo" && kind != "contradiction" {
		return ErrNotActionable
	}
	if (status == "closed") == closed {
		return tx.Commit()
	}
	comment, err = validateStateChange(kind, closed, comment)
	if err != nil {
		return err
	}
	next, eventKind := "open", "reopened"
	if closed {
		next, eventKind = "closed", "closed"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE thinking_cards SET status=$1,closed_at=CASE WHEN $2 THEN NOW() ELSE NULL END,updated_at=NOW() WHERE id=$3`, next, closed, cardID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO thinking_card_events (card_id,user_id,kind,body) VALUES ($1,$2,$3,$4)`, cardID, uid, eventKind, comment); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE thinking_projects SET context_updated_at=NOW(),updated_at=NOW() WHERE id=$1 AND user_id=$2`, projectID, uid); err != nil {
		return err
	}
	return tx.Commit()
}

func LoadEvents(ctx context.Context, d *db.DB, uid string, cardID int64) ([]Event, error) {
	var exists int
	if err := d.QueryRowContext(ctx, `SELECT 1 FROM thinking_cards WHERE id=$1 AND user_id=$2`, cardID, uid).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := d.QueryContext(ctx, `SELECT id,card_id,kind,body,created_at FROM thinking_card_events WHERE card_id=$1 AND user_id=$2 ORDER BY id`, cardID, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.CardID, &event.Kind, &event.Body, &event.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func LoadProjectEvents(ctx context.Context, d *db.DB, uid string, projectID int64) (map[int64][]Event, error) {
	// AI gets recent discussion plus every close/reopen explanation. The full
	// thread remains available to the user through LoadEvents.
	rows, err := d.QueryContext(ctx, `
		SELECT id,card_id,kind,body,created_at FROM (
			SELECT e.id,e.card_id,e.kind,e.body,e.created_at,
			       ROW_NUMBER() OVER (PARTITION BY e.card_id,e.kind ORDER BY e.id DESC) AS kind_rank
			FROM thinking_card_events e
			JOIN thinking_cards c ON c.id=e.card_id
			WHERE c.project_id=$1 AND c.user_id=$2 AND e.user_id=$2
		) events
		WHERE kind<>'comment' OR kind_rank<=10
		ORDER BY id`, projectID, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]Event)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.CardID, &event.Kind, &event.Body, &event.CreatedAt); err != nil {
			return nil, err
		}
		out[event.CardID] = append(out[event.CardID], event)
	}
	return out, rows.Err()
}
