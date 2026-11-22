package ai

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/genai"
)

// price is a model's list price, USD per million tokens. Thinking is
// billed as output.
type price struct{ input, cached, output float64 }

// flashDoubles is when Gemini 3.x Flash list prices double.
var flashDoubles = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

// priceOf is the list price of model at t (Gemini API paid tier, standard).
// Used to log and budget each call; keep it in step with Google's pricing
// page. An unknown model is priced as Hydrogen and logged.
func priceOf(model string, t time.Time) price {
	switch {
	case model == decisionModel:
		return price{input: 0.10} // no output tokens
	case strings.HasPrefix(model, "gemini-4-hydrogen"):
		return price{input: 1.50, cached: 0.075, output: 7.50}
	case strings.HasPrefix(model, "gemini-3.8-flash"), strings.HasPrefix(model, "gemini-3.6-flash"):
		if t.Before(flashDoubles) {
			return price{input: 0.75, cached: 0.075, output: 3.75}
		}
		return price{input: 1.50, cached: 0.15, output: 7.50}
	}
	unknownPrice.Do(func() { log.Warn().Str("model", model).Msg("ai price unknown, using Hydrogen's") })
	return price{input: 1.50, cached: 0.075, output: 7.50}
}

var unknownPrice sync.Once

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

// CostUSD estimates what the calls cost at model's list price at t.
func (u usage) CostUSD(model string, t time.Time) float64 {
	p := priceOf(model, t)
	fresh := u.Prompt - u.Cached
	if fresh < 0 {
		fresh = 0
	}
	return (float64(fresh)*p.input + float64(u.Cached)*p.cached +
		float64(u.Output+u.Thinking)*p.output) / 1e6
}

// CachePct is the share of input served from the cache.
func (u usage) CachePct() int {
	if u.Prompt == 0 {
		return 0
	}
	return u.Cached * 100 / u.Prompt
}

// fields adds the token counts and cost (at model's price) to a log line.
func (u usage) fields(e *zerolog.Event, model string) *zerolog.Event {
	return e.Str("model", model).Int("in_tokens", u.Prompt).
		Int("cached_tokens", u.Cached).
		Int("cache_pct", u.CachePct()).
		Int("out_tokens", u.Output).
		Int("thinking_tokens", u.Thinking).
		Float64("cost_usd", roundCost(u.CostUSD(model, time.Now())))
}

func finishOf(resp *genai.GenerateContentResponse) string {
	if resp == nil || len(resp.Candidates) == 0 {
		return ""
	}
	return string(resp.Candidates[0].FinishReason)
}

func roundCost(v float64) float64 { return float64(int64(v*1e6+0.5)) / 1e6 }

// generate is one non-streaming model call, logged with its tokens and
// cost under kind ("categorize", "parse_text", …).
func (s *Service) generate(ctx context.Context, kind string, contents []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	if err := s.gate(ctx, "", kind); err != nil {
		return nil, err
	}
	start := time.Now()
	model := s.modelFor(kind)
	resp, err := s.client.GenerateContent(ctx, model, contents, cfg)
	var u usage
	if resp != nil {
		u = usageOf(resp.UsageMetadata)
	}
	s.spent(ctx, "", kind, model, u)
	ms := time.Since(start).Milliseconds()
	ev := log.Info()
	if err != nil {
		ev = log.Warn().Err(err)
	}
	u.fields(ev.Str("kind", kind).Int64("ms", ms), model).Msg("ai call")
	var sys *genai.Content
	var decls []*genai.FunctionDeclaration
	if cfg != nil {
		sys = cfg.SystemInstruction
		for _, t := range cfg.Tools {
			decls = append(decls, t.FunctionDeclarations...)
		}
	}
	s.record(ctx, callRecord{
		Kind: kind, Model: model, Thinking: thinkingOf(cfg), Usage: u, At: start,
		Parts: scaleParts(map[string]int{
			"system": contentChars(sys), "tools": declChars(decls), "message": contentChars(contents...),
		}, u.Prompt),
		Tools: len(decls), Finish: finishOf(resp), Err: err, Ms: ms,
	})
	return resp, err
}
