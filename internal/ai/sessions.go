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
func LoadSession(ctx context.Context, d *db.DB, uid string, sid int64) (*Session, error) {
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
	s.Messages = SanitizeHistory(s.Messages)
	return &s, nil
}

// ListSessions returns recent sessions (metadata only).
func ListSessions(ctx context.Context, d *db.DB, uid string) ([]SessionMeta, error) {
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
func CreateSession(ctx context.Context, d *db.DB, uid string, title string) (int64, error) {
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
func SaveSessionMessages(ctx context.Context, d *db.DB, uid string, sid int64, messages []*genai.Content) error {
	trimmed := messages
	if len(trimmed) > historyWindow*2 {
		trimmed = trimmed[len(trimmed)-historyWindow*2:]
	}
	// SanitizeHistory drops orphan tool-call / tool-response pairs and invalid
	// parts so the next chat round never starts with a malformed history.
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
		WHERE id = $3 AND user_id = $4`, raw, deriveTitle(trimmed), sid, uid); err != nil {
		return err
	}
	return nil
}

// DeleteSession removes a conversation.
func DeleteSession(ctx context.Context, d *db.DB, uid string, sid int64) error {
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

// isPartValid returns true if the part contains valid payload for Gemini
// content (oneof data must be set). Thought-only or empty parts are invalid.
func isPartValid(p *genai.Part) bool {
	if p == nil || p.Thought {
		return false
	}
	if p.Text != "" {
		return true
	}
	return p.FunctionCall != nil ||
		p.FunctionResponse != nil ||
		p.InlineData != nil ||
		p.FileData != nil ||
		p.ExecutableCode != nil ||
		p.CodeExecutionResult != nil
}

func sanitizeParts(parts []*genai.Part) []*genai.Part {
	if len(parts) == 0 {
		return nil
	}
	var out []*genai.Part
	for _, p := range parts {
		if !isPartValid(p) {
			continue
		}
		// If both this and previous part are plain text, merge them.
		if len(out) > 0 && out[len(out)-1].Text != "" && p.Text != "" &&
			out[len(out)-1].FunctionCall == nil && p.FunctionCall == nil &&
			out[len(out)-1].FunctionResponse == nil && p.FunctionResponse == nil {
			out[len(out)-1].Text += p.Text
			continue
		}
		cp := *p
		if p.FunctionCall != nil {
			call := *p.FunctionCall
			cp.FunctionCall = &call
		}
		if p.FunctionResponse != nil {
			response := *p.FunctionResponse
			cp.FunctionResponse = &response
		}
		cp.ThoughtSignature = append([]byte(nil), p.ThoughtSignature...)
		out = append(out, &cp)
	}
	return out
}

func hasFunctionCall(c *genai.Content) bool {
	if c == nil || c.Role != "model" {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

func hasFunctionResponse(c *genai.Content) bool {
	if c == nil || c.Role != "user" {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// functionPairMatches validates every response against one call. Gemini 3
// adds call IDs; legacy stored sessions only have names, so repair a missing
// response ID when the name still identifies the corresponding call.
func functionPairMatches(callTurn, responseTurn *genai.Content) bool {
	if !hasFunctionCall(callTurn) || !hasFunctionResponse(responseTurn) {
		return false
	}
	var calls []*genai.FunctionCall
	var responses []*genai.FunctionResponse
	for _, p := range callTurn.Parts {
		if p != nil && p.FunctionCall != nil {
			calls = append(calls, p.FunctionCall)
		}
	}
	for _, p := range responseTurn.Parts {
		if p != nil && p.FunctionResponse != nil {
			responses = append(responses, p.FunctionResponse)
		}
	}
	if len(calls) == 0 || len(calls) != len(responses) {
		return false
	}
	used := make([]bool, len(responses))
	for _, call := range calls {
		matched := -1
		for index, response := range responses {
			if used[index] || call.Name == "" || response.Name != call.Name {
				continue
			}
			if call.ID != "" && response.ID != "" && response.ID != call.ID {
				continue
			}
			matched = index
			break
		}
		if matched < 0 {
			return false
		}
		used[matched] = true
		if responses[matched].ID == "" {
			responses[matched].ID = call.ID
		}
	}
	return true
}

// alternatingHistorySuffix keeps the newest complete, valid conversation
// suffix. This is deliberately loss-tolerant: retaining a little less context
// is better than making every future turn fail on a legacy malformed row.
func alternatingHistorySuffix(history []*genai.Content) []*genai.Content {
	end := len(history)
	for end > 0 && history[end-1].Role != "model" {
		end--
	}
	for start := 0; start < end; start++ {
		if history[start].Role != "user" || hasFunctionResponse(history[start]) || (end-start)%2 != 0 {
			continue
		}
		valid := true
		for index := start; index < end; index++ {
			want := "user"
			if (index-start)%2 == 1 {
				want = "model"
			}
			if history[index].Role != want {
				valid = false
				break
			}
		}
		if valid {
			return history[start:end]
		}
	}
	return []*genai.Content{}
}

// SanitizeHistory walks the conversation and:
//
//  1. Strips nil parts, empty parts, thought parts, and empty content turns.
//  2. Merges fragmented text parts within the same turn.
//  3. Enforces Gemini's strict tool-pair contract (drops orphan or mismatched
//     function responses and dangling function calls).
//  4. Keeps the newest suffix that starts with user and strictly alternates
//     user/model roles.
func SanitizeHistory(history []*genai.Content) []*genai.Content {
	if len(history) == 0 {
		return history
	}

	var cleaned []*genai.Content
	for _, c := range history {
		if c == nil || (c.Role != "user" && c.Role != "model") {
			continue
		}
		validParts := sanitizeParts(c.Parts)
		if len(validParts) == 0 {
			continue
		}
		cleaned = append(cleaned, &genai.Content{
			Role:  c.Role,
			Parts: validParts,
		})
	}

	if len(cleaned) == 0 {
		return []*genai.Content{}
	}

	var validated []*genai.Content
	for i := 0; i < len(cleaned); i++ {
		c := cleaned[i]
		if hasFunctionResponse(c) {
			if len(validated) == 0 || !functionPairMatches(validated[len(validated)-1], c) {
				continue
			}
			validated = append(validated, c)
			continue
		}

		if hasFunctionCall(c) {
			if i+1 < len(cleaned) && functionPairMatches(c, cleaned[i+1]) {
				validated = append(validated, c)
			} else {
				var nonCallParts []*genai.Part
				for _, p := range c.Parts {
					if p.FunctionCall == nil {
						nonCallParts = append(nonCallParts, p)
					}
				}
				if len(nonCallParts) > 0 {
					validated = append(validated, &genai.Content{
						Role:  c.Role,
						Parts: nonCallParts,
					})
				}
			}
			continue
		}

		validated = append(validated, c)
	}

	return alternatingHistorySuffix(validated)
}

// deriveTitle picks the first 8 words of the first user-text message
// as a title. Falls back to a timestamp.
func deriveTitle(messages []*genai.Content) string {
	for _, c := range messages {
		if c == nil || c.Role != "user" {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.Text == "" {
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
