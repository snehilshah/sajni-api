package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/genai"
)

// The usage ledger (table ai_calls): one row per model call, kept a year,
// so token spend can be read by feature, model and prompt part long after
// the 30-day logs are gone. Prompts themselves are never stored.

// callRecord is one model call.
type callRecord struct {
	UID      string
	Kind     string // chat, palette, parse_text, categorize, …
	Model    string
	Thinking string // low / medium / high; "" for the Decisions API
	Turn     string // groups a chat question's rounds
	Round    int
	Usage    usage
	At       time.Time
	// Parts estimates the input tokens by part of the prompt (system,
	// tools, now, history, turn, message): characters per part, scaled
	// to the input tokens Gemini reported.
	Parts map[string]int
	// Tools is how many tool declarations were sent; Groups the tool
	// groups loaded; Calls the tools the model called this round.
	Tools  int
	Groups []string
	Calls  []string
	// Results is the estimated tokens each called tool returned (they're
	// input on the next round).
	Results    map[string]int
	Finish     string
	Confidence *float64 // Decisions API: the top option's probability
	Err        error
	Ms         int64
}

func newTurnID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// modelFor is the model for a kind of call.
func (s *Service) modelFor(kind string) string {
	if m := s.modelByKind[kind]; m != "" {
		return m
	}
	return s.model
}

// record writes rec to the ledger off the request path: a slow or failed
// write never delays or fails the AI call itself.
func (s *Service) record(ctx context.Context, rec callRecord) {
	if s.db == nil {
		return
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	uid := userOf(ctx, rec.UID)
	cost := rec.Usage.CostUSD(rec.Model, rec.At)
	parts, _ := json.Marshal(rec.Parts)
	results, _ := json.Marshal(rec.Results)
	errText := ""
	if rec.Err != nil {
		errText = rec.Err.Error()
		if len(errText) > 300 {
			errText = errText[:300]
		}
	}
	go func() {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var user any
		if uid != "" {
			user = uid
		}
		if _, err := s.db.ExecContext(wctx, `
			INSERT INTO ai_calls (user_id, at, kind, model, thinking, turn_id, round,
			                      in_tokens, cached_tokens, out_tokens, thinking_tokens, cost_usd,
			                      parts, tools_declared, groups, calls, results, finish, confidence, ok, error, ms)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,0),$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,NULLIF($18,''),$19,$20,NULLIF($21,''),$22)`,
			user, rec.At, rec.Kind, rec.Model, rec.Thinking, rec.Turn, rec.Round,
			rec.Usage.Prompt, rec.Usage.Cached, rec.Usage.Output, rec.Usage.Thinking, roundCost(cost),
			nullJSON(parts), rec.Tools, nonNil(rec.Groups), nonNil(rec.Calls), nullJSON(results), rec.Finish,
			rec.Confidence, rec.Err == nil, errText, rec.Ms); err != nil {
			log.Warn().Err(err).Str("kind", rec.Kind).Msg("ai ledger write failed")
		}
	}()
}

func nullJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" || string(b) == "{}" {
		return nil
	}
	return string(b)
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// contentChars is the size of contents as Gemini receives them: text,
// function calls and function results.
func contentChars(cs ...*genai.Content) int {
	n := 0
	for _, c := range cs {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			n += len(p.Text)
			if p.FunctionCall != nil {
				b, _ := json.Marshal(p.FunctionCall.Args)
				n += len(p.FunctionCall.Name) + len(b)
			}
			if p.FunctionResponse != nil {
				b, _ := json.Marshal(p.FunctionResponse.Response)
				n += len(p.FunctionResponse.Name) + len(b)
			}
		}
	}
	return n
}

func declChars(decls []*genai.FunctionDeclaration) int {
	if len(decls) == 0 {
		return 0
	}
	b, _ := json.Marshal(decls)
	return len(b)
}

// scaleParts turns characters per part into tokens, in proportion, so the
// parts add up to the input tokens Gemini reported.
func scaleParts(chars map[string]int, promptTokens int) map[string]int {
	total := 0
	for _, n := range chars {
		total += n
	}
	if total == 0 || promptTokens == 0 {
		return nil
	}
	out := make(map[string]int, len(chars))
	for k, n := range chars {
		if n > 0 {
			out[k] = (n*promptTokens + total/2) / total
		}
	}
	return out
}

// thinkingOf names cfg's thinking level for the ledger.
func thinkingOf(cfg *genai.GenerateContentConfig) string {
	if cfg == nil || cfg.ThinkingConfig == nil {
		return ""
	}
	switch cfg.ThinkingConfig.ThinkingLevel {
	case genai.ThinkingLevelLow:
		return "low"
	case genai.ThinkingLevelMedium:
		return "medium"
	case genai.ThinkingLevelHigh:
		return "high"
	}
	return string(cfg.ThinkingConfig.ThinkingLevel)
}
