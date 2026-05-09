package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"google.golang.org/genai"

	"sajni/internal/ai"
)

// registerAIRoutes mounts /api/ai/* on the protected mux. If the AI
// service is nil (no GEMINI_API_KEY configured), every endpoint
// responds with 503 so the frontend can hide the affordance gracefully.
func registerAIRoutes(mux *http.ServeMux, deps Deps, svc *ai.Service) {
	limiter := newAILimiter()

	disabled := func(w http.ResponseWriter) {
		errJSON(w, http.StatusServiceUnavailable, "AI is not configured on this server")
	}

	mux.HandleFunc("GET /api/ai/status", func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeJSON(w, 200, map[string]any{"enabled": false})
			return
		}
		writeJSON(w, 200, map[string]any{"enabled": true, "model": svc.Model()})
	})

	mux.HandleFunc("GET /api/ai/sessions", func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			disabled(w)
			return
		}
		uid := userID(r.Context())
		list, err := ai.ListSessions(r.Context(), deps.DB, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, list)
	})

	mux.HandleFunc("GET /api/ai/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			disabled(w)
			return
		}
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "bad id")
			return
		}
		sess, err := ai.LoadSession(r.Context(), deps.DB, uid, id)
		if err != nil {
			errJSON(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, sess)
	})

	mux.HandleFunc("POST /api/ai/sessions", func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			disabled(w)
			return
		}
		uid := userID(r.Context())
		id, err := ai.CreateSession(r.Context(), deps.DB, uid, "New chat")
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	})

	// Adopt a one-shot palette exchange into a real chat session so the
	// user can keep the conversation going from the sidebar.
	mux.HandleFunc("POST /api/ai/sessions/adopt", func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			disabled(w)
			return
		}
		uid := userID(r.Context())
		var body struct {
			History []*genai.Content `json:"history"`
			Title   string           `json:"title"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		id, err := ai.CreateSession(r.Context(), deps.DB, uid, body.Title)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		if len(body.History) > 0 {
			_ = ai.SaveSessionMessages(r.Context(), deps.DB, uid, id, body.History)
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	})

	mux.HandleFunc("DELETE /api/ai/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			disabled(w)
			return
		}
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "bad id")
			return
		}
		if err := ai.DeleteSession(r.Context(), deps.DB, uid, id); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /api/ai/chat", func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			disabled(w)
			return
		}
		uid := userID(r.Context())
		if !limiter.allow(uid) {
			errJSON(w, 429, "rate limit exceeded")
			return
		}

		var body struct {
			SessionID int64  `json:"session_id"`
			Message   string `json:"message"`
			Mode      string `json:"mode"` // "palette" or "chat"
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Message == "" {
			errJSON(w, 400, "missing message")
			return
		}
		if body.Mode == "" {
			body.Mode = "chat"
		}

		var prior []*genai.Content
		if body.SessionID > 0 {
			sess, err := ai.LoadSession(r.Context(), deps.DB, uid, body.SessionID)
			if err == nil {
				prior = sess.Messages
			}
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			errJSON(w, 500, "streaming unsupported")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)
		flusher.Flush()

		// Detached context with a hard timeout — Cloud Run also caps at 60s.
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
		defer cancel()

		req := ai.ChatRequest{
			UserID:  uid,
			Mode:    body.Mode,
			Message: body.Message,
			History: ai.TrimHistory(prior),
		}

		var finalHistory []*genai.Content
		for ev := range svc.Chat(ctx, req) {
			if ev.Type == "done" {
				var raw struct {
					Text    string           `json:"text"`
					History []*genai.Content `json:"history"`
				}
				_ = json.Unmarshal(ev.Data, &raw)
				finalHistory = raw.History
			}
			if err := writeSSE(w, ev.Type, ev.Data); err != nil {
				return
			}
			flusher.Flush()
		}

		// Persist whenever a session id is supplied, regardless of mode.
		// Background ctx because request ctx may be cancelled at stream end.
		if body.SessionID > 0 && len(finalHistory) > 0 {
			_ = ai.SaveSessionMessages(context.Background(), deps.DB, uid, body.SessionID, finalHistory)
		}
	})
}

// writeSSE writes one Server-Sent-Event frame.
func writeSSE(w http.ResponseWriter, event string, data json.RawMessage) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	payload := []byte(data)
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	return nil
}

// ----- per-user rate limiter -----

type aiLimiter struct {
	mu     sync.Mutex
	bucket map[int64]*aiBucket
}

type aiBucket struct {
	tokens   int
	last     time.Time
	capacity int
	rate     time.Duration
}

func newAILimiter() *aiLimiter {
	return &aiLimiter{bucket: map[int64]*aiBucket{}}
}

// allow returns true if the user has tokens to spend. ~10 req/min/user.
func (l *aiLimiter) allow(uid int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.bucket[uid]
	if !ok {
		b = &aiBucket{tokens: 10, capacity: 10, rate: 6 * time.Second, last: time.Now()}
		l.bucket[uid] = b
	}
	now := time.Now()
	add := int(now.Sub(b.last) / b.rate)
	if add > 0 {
		b.tokens += add
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}
