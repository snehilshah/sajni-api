package ai

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/genai"
)

// Gemini 3.8 Flash list prices, USD per million tokens (to 2026-12-31; they
// double from 2027-01-01). Thinking is billed as output; cached input is
// a tenth of fresh input. Used only to log an estimate per call.
const (
	priceInput  = 0.75
	priceCached = 0.075
	priceOutput = 3.75
)

// usage is the token count of one or more model calls.
type usage struct {
	Prompt   int // all input tokens, cached ones included
	Cached   int // input tokens served from Gemini's cache
	Output   int // visible reply and function calls
	Thinking int
}

func usageOf(m *genai.GenerateContentResponseUsageMetadata) usage {
	if m == nil {
		return usage{}
	}
	return usage{
		Prompt:   int(m.PromptTokenCount) + int(m.ToolUsePromptTokenCount),
		Cached:   int(m.CachedContentTokenCount),
		Output:   int(m.CandidatesTokenCount),
		Thinking: int(m.ThoughtsTokenCount),
	}
}

func (u *usage) add(o usage) {
	u.Prompt += o.Prompt
	u.Cached += o.Cached
	u.Output += o.Output
	u.Thinking += o.Thinking
}

// Total is every token billed, cached input included.
func (u usage) Total() int { return u.Prompt + u.Output + u.Thinking }

// CostUSD estimates what the calls cost at list price.
func (u usage) CostUSD() float64 {
	fresh := u.Prompt - u.Cached
	if fresh < 0 {
		fresh = 0
	}
	return (float64(fresh)*priceInput + float64(u.Cached)*priceCached +
		float64(u.Output+u.Thinking)*priceOutput) / 1e6
}

// CachePct is the share of input served from the cache.
func (u usage) CachePct() int {
	if u.Prompt == 0 {
		return 0
	}
	return u.Cached * 100 / u.Prompt
}

// fields adds the token counts and cost to a log line.
func (u usage) fields(e *zerolog.Event) *zerolog.Event {
	return e.Int("in_tokens", u.Prompt).
		Int("cached_tokens", u.Cached).
		Int("cache_pct", u.CachePct()).
		Int("out_tokens", u.Output).
		Int("thinking_tokens", u.Thinking).
		Float64("cost_usd", roundCost(u.CostUSD()))
}

func roundCost(v float64) float64 { return float64(int64(v*1e6+0.5)) / 1e6 }

// generate is one non-streaming model call, logged with its tokens and
// cost under kind ("categorize", "parse_text", …).
func (s *Service) generate(ctx context.Context, kind string, contents []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	start := time.Now()
	resp, err := s.client.GenerateContent(ctx, s.model, contents, cfg)
	var u usage
	if resp != nil {
		u = usageOf(resp.UsageMetadata)
	}
	ev := log.Info()
	if err != nil {
		ev = log.Warn().Err(err)
	}
	u.fields(ev.Str("kind", kind).Str("model", s.model).Int64("ms", time.Since(start).Milliseconds())).
		Msg("ai call")
	return resp, err
}
