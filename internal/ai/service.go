// Package ai houses the Gemini-backed agent that powers the @sajni
// command-palette mode and the sidebar chat panel.
//
// The shape is intentionally small: one Service, one Chat method that
// returns a stream of typed Events, one tool registry, one
// system-prompt builder. Everything else (HTTP framing, persistence,
// rate limiting) lives in internal/api/ai.go.
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/genai"

	"sajni/internal/db"
	"sajni/internal/storage"
)

const (
	// gemini-2.5-flash is the cheapest tier that *reliably* continues
	// after function calls. The -lite tier is ~3× cheaper but tends to
	// stop silently after a tool round, which makes the agent loop
	// useless. Override with GEMINI_MODEL if you find a cheaper model
	// that handles function calling well.
	defaultModel = "gemini-2.5-flash"
	// Tool budget. A typical palette answer needs: get_current_context →
	// list_* → maybe cross-check → final text. Bumping these to 8/6 cuts
	// the "exceeded max tool rounds" rate on multi-step palette asks
	// without changing per-request cost meaningfully.
	maxToolRounds   = 8
	paletteRounds   = 6
	maxOutputTokens = 1024
	temperature     = 0.4
)

// Event is one item pushed onto the SSE stream during a chat turn.
// Type is one of:
//
//	delta        — incremental text from the model
//	tool_call    — model invoked a tool ({name, args})
//	tool_result  — server executed the tool ({name, ok, error?, meta?})
//	error        — fatal error; stream ends
//	done         — turn complete ({history, text})
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

func newEvent(t string, v any) Event {
	b, _ := json.Marshal(v)
	return Event{Type: t, Data: b}
}

// Service is the AI runtime. A nil *Service means the feature is
// disabled (no GEMINI_API_KEY). Construct once at startup; reuse per
// request.
type Service struct {
	db     *db.DB
	store  storage.Storage
	client *genai.Client
	model  string
	tools  []Tool
}

// NewService initializes a Gemini client. Returns (nil, nil) if no
// GEMINI_API_KEY is set — the HTTP layer should treat that as a 503.
func NewService(ctx context.Context, database *db.DB, store storage.Storage) (*Service, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil, nil
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  key,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini client: %w", err)
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = defaultModel
	}
	s := &Service{
		db:     database,
		store:  store,
		client: client,
		model:  model,
	}
	s.tools = s.buildTools()
	return s, nil
}

// Model returns the active model id (for debug/logging).
func (s *Service) Model() string { return s.model }

// ChatRequest is the input for one user turn.
type ChatRequest struct {
	UserID  int64
	Mode    string // "palette" or "chat"
	Message string
	// History is the trimmed conversation prior to this turn. May be nil.
	History []*genai.Content
}

// ChatResult is what Chat returns alongside the event stream — the
// caller reads events until the channel closes, then uses the final
// History for persistence.
type ChatResult struct {
	History []*genai.Content
	Text    string
}

// Chat runs the agent loop. It returns a channel of Events; consumers
// read until close. The full message history is also pushed via a final
// `done` event (so the HTTP layer can persist it without a separate
// callback).
func (s *Service) Chat(ctx context.Context, req ChatRequest) <-chan Event {
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		s.run(ctx, req, out)
	}()
	return out
}

func (s *Service) run(ctx context.Context, req ChatRequest, out chan<- Event) {
	send := func(t string, v any) {
		select {
		case out <- newEvent(t, v):
		case <-ctx.Done():
		}
	}

	contents := append([]*genai.Content(nil), req.History...)
	contents = append(contents, &genai.Content{
		Role:  "user",
		Parts: []*genai.Part{{Text: req.Message}},
	})

	sysPrompt := s.buildSystemInstruction(ctx, req.UserID)

	decls := make([]*genai.FunctionDeclaration, len(s.tools))
	for i, t := range s.tools {
		decls[i] = &genai.FunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Schema,
		}
	}
	temp := float32(temperature)
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: sysPrompt}},
		},
		Tools:           []*genai.Tool{{FunctionDeclarations: decls}},
		MaxOutputTokens: maxOutputTokens,
		Temperature:     &temp,
	}

	rounds := maxToolRounds
	if req.Mode == "palette" {
		rounds = paletteRounds
	}

	var finalText strings.Builder

	for r := 0; r < rounds; r++ {
		var (
			turnText  strings.Builder
			modelPart []*genai.Part
			calls     []*genai.FunctionCall
			streamErr error
		)

		for resp, err := range s.client.Models.GenerateContentStream(ctx, s.model, contents, cfg) {
			if err != nil {
				streamErr = err
				break
			}
			if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
				continue
			}
			for _, p := range resp.Candidates[0].Content.Parts {
				if p.Text != "" && !p.Thought {
					turnText.WriteString(p.Text)
					send("delta", map[string]string{"text": p.Text})
				}
				if p.FunctionCall != nil {
					calls = append(calls, p.FunctionCall)
				}
				modelPart = append(modelPart, p)
			}
		}
		if streamErr != nil {
			send("error", map[string]string{"message": streamErr.Error()})
			return
		}

		finalText.WriteString(turnText.String())

		if len(modelPart) > 0 {
			contents = append(contents, &genai.Content{Role: "model", Parts: modelPart})
		}

		if len(calls) == 0 {
			send("done", map[string]any{
				"text":    finalText.String(),
				"history": contents,
			})
			return
		}

		respParts := make([]*genai.Part, 0, len(calls))
		for _, fc := range calls {
			send("tool_call", map[string]any{"name": fc.Name, "args": fc.Args})
			data, meta, err := s.dispatch(ctx, req.UserID, fc.Name, fc.Args)
			res := map[string]any{"name": fc.Name, "ok": err == nil}
			if err != nil {
				res["error"] = err.Error()
			}
			if meta != nil {
				res["meta"] = meta
			}
			send("tool_result", res)

			respObj := map[string]any{}
			if err != nil {
				respObj["error"] = err.Error()
			} else {
				respObj["result"] = data
			}
			respParts = append(respParts, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					Name:     fc.Name,
					Response: respObj,
				},
			})
		}
		contents = append(contents, &genai.Content{Role: "user", Parts: respParts})
	}

	// Agent ran the full tool budget without converging on a final text
	// reply. If we have *any* partial text, send it as `done` so the
	// palette/sidebar shows something useful instead of "no response
	// yet". Otherwise surface a friendly error.
	if finalText.Len() > 0 {
		send("done", map[string]any{
			"text":      finalText.String(),
			"history":   contents,
			"truncated": true,
		})
		return
	}
	send("error", map[string]string{
		"message": "I couldn't finish that one in time — try the sidebar chat for deeper questions.",
	})
}
