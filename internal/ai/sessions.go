package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"sajni/internal/db"
)

// Session is one conversation thread. Messages is the rolling history
// (mirrors genai.Content shape).
type Session struct {
	ID        int64            `json:"id"`
	Title     string           `json:"title"`
	Messages  []*genai.Content `json:"messages"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
}

// SessionMeta is the lightweight shape used for sidebar lists.
type SessionMeta struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

const historyWindow = 20

// LoadSession reads the full history for a session, scoped to the user.
func LoadSession(ctx context.Context, d *db.DB, uid, sid int64) (*Session, error) {
	var s Session
	var raw []byte
	err := d.QueryRowContext(ctx, `
		SELECT id, title, messages, created_at, updated_at
		FROM ai_sessions WHERE id=$1 AND user_id=$2`, sid, uid).
		Scan(&s.ID, &s.Title, &raw, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("session not found")
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.Messages); err != nil {
			s.Messages = nil
		}
	}
	if s.Messages == nil {
		s.Messages = []*genai.Content{}
	}
	return &s, nil
}

// ListSessions returns recent sessions (metadata only).
func ListSessions(ctx context.Context, d *db.DB, uid int64) ([]SessionMeta, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, title, created_at, updated_at FROM ai_sessions
		WHERE user_id=$1 ORDER BY updated_at DESC LIMIT 50`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionMeta{}
	for rows.Next() {
		var m SessionMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.CreatedAt, &m.UpdatedAt); err == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// CreateSession inserts an empty session and returns its id.
func CreateSession(ctx context.Context, d *db.DB, uid int64, title string) (int64, error) {
	if title == "" {
		title = "New chat"
	}
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO ai_sessions (user_id, title, messages) VALUES ($1, $2, '[]'::jsonb)
		RETURNING id`, uid, title).Scan(&id)
	return id, err
}

// SaveSessionMessages persists the trimmed history. Auto-titles the
// session from the first user message if it's still "New chat".
func SaveSessionMessages(ctx context.Context, d *db.DB, uid, sid int64, messages []*genai.Content) error {
	trimmed := messages
	if len(trimmed) > historyWindow*2 {
		trimmed = trimmed[len(trimmed)-historyWindow*2:]
	}
	// SanitizeHistory drops orphan tool-call / tool-response pairs so the
	// next chat round never starts with a malformed history.
	trimmed = SanitizeHistory(trimmed)
	raw, err := json.Marshal(trimmed)
	if err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, `
		UPDATE ai_sessions
		SET messages = $1::jsonb,
		    title = CASE WHEN title = 'New chat' OR title = '' THEN $2 ELSE title END,
		    updated_at = NOW()
		WHERE id = $3 AND user_id = $4`, raw, deriveTitle(messages), sid, uid); err != nil {
		return err
	}
	return nil
}

// DeleteSession removes a conversation.
func DeleteSession(ctx context.Context, d *db.DB, uid, sid int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM ai_sessions WHERE id=$1 AND user_id=$2`, sid, uid)
	return err
}

// TrimHistory returns the last 2*historyWindow entries — keeps the
// agent's working context bounded. The result is then run through
// SanitizeHistory so we never hand Gemini a slice that starts with an
// orphan function-response or ends with a dangling function-call,
// which is what triggers
//
//	"Please ensure that function response turn comes immediately
//	 after a function call turn."
func TrimHistory(history []*genai.Content) []*genai.Content {
	out := history
	if len(out) > historyWindow*2 {
		out = out[len(out)-historyWindow*2:]
	}
	return SanitizeHistory(out)
}

// SanitizeHistory walks the conversation and strips orphan tool turns:
//
//  1. A leading user-turn whose parts include any function_response.
//     This happens after TrimHistory slices in the middle of a
//     model(function_call) → user(function_response) pair.
//  2. A trailing model-turn that has any function_call but no matching
//     user(function_response) immediately after.
//
// The remaining turns satisfy Gemini's strict tool-pair contract.
func SanitizeHistory(history []*genai.Content) []*genai.Content {
	if len(history) == 0 {
		return history
	}
	// (1) skip orphan function-response turns at the head.
	start := 0
	for start < len(history) {
		c := history[start]
		if c == nil {
			start++
			continue
		}
		hasFnResp := false
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil {
				hasFnResp = true
				break
			}
		}
		if c.Role == "user" && hasFnResp {
			start++
			continue
		}
		break
	}
	out := history[start:]

	// (2) drop trailing dangling function-call turn.
	for len(out) > 0 {
		last := out[len(out)-1]
		if last == nil {
			out = out[:len(out)-1]
			continue
		}
		hasFnCall := false
		for _, p := range last.Parts {
			if p != nil && p.FunctionCall != nil {
				hasFnCall = true
				break
			}
		}
		if last.Role == "model" && hasFnCall {
			out = out[:len(out)-1]
			continue
		}
		break
	}
	return out
}

// deriveTitle picks the first 8 words of the first user-text message
// as a title. Falls back to a timestamp.
func deriveTitle(messages []*genai.Content) string {
	for _, c := range messages {
		if c == nil || c.Role != "user" {
			continue
		}
		for _, p := range c.Parts {
			if p.Text == "" {
				continue
			}
			words := strings.Fields(p.Text)
			if len(words) > 8 {
				words = words[:8]
			}
			return strings.TrimSpace(strings.Join(words, " "))
		}
	}
	return time.Now().Format("Jan 2 15:04")
}
