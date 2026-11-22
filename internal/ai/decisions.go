package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// The Gemini Decisions API (gemini-decision-1): closed-set questions
// answered with calibrated probabilities, about 0.2 s, $0.10 per million
// input tokens and no output tokens. REST only (the Go SDK has no client).
// https://aistudio.google.com/docs/decisions-api
const (
	decisionModel     = "gemini-decision-1"
	decisionsEndpoint = "https://generativelanguage.googleapis.com/v1beta/decisions"
	decisionTimeout   = 4 * time.Second
)

var decisionHTTP = &http.Client{Timeout: decisionTimeout}

// choose asks one "choice" question about input: which of options (id →
// description, "" when the id says it all) fits. Returns the chosen id
// and its probability. Gated, budgeted and recorded like any model call.
func (s *Service) choose(ctx context.Context, kind, input, instructions string, options map[string]string) (string, float64, error) {
	if s.apiKey == "" || s.decisionsURL == "" {
		return "", 0, errors.New("decisions api not configured")
	}
	if len(options) < 2 || len(options) > 255 {
		return "", 0, fmt.Errorf("decisions api: %d options (2–255)", len(options))
	}
	if err := s.gate(ctx, "", kind); err != nil {
		return "", 0, err
	}
	body, _ := json.Marshal(map[string]any{
		"model": decisionModel,
		"input": input,
		"questions": map[string]any{"q": map[string]any{
			"type": "choice", "instructions": instructions, "criteria": options,
		}},
	})
	start := time.Now()
	choice, conf, u, err := s.postDecision(ctx, body)
	s.spent(ctx, "", kind, decisionModel, u)
	ms := time.Since(start).Milliseconds()
	ev := s.logEvent(err)
	u.fields(ev.Str("kind", kind).Int64("ms", ms).Str("choice", choice).Float64("confidence", conf), decisionModel).Msg("ai call")
	rec := callRecord{Kind: kind, Model: decisionModel, Usage: u, At: start, Err: err, Ms: ms}
	if err == nil {
		rec.Confidence = &conf
	}
	s.record(ctx, rec)
	return choice, conf, err
}

func (s *Service) logEvent(err error) *zerolog.Event {
	if err != nil {
		return log.Warn().Err(err)
	}
	return log.Info()
}

// TxnFacts is what's known about a transaction when choosing its category.
type TxnFacts struct {
	Type        string // expense | income
	Amount      float64
	Description string // the payee or shop as read
	Note        string
	Message     string // the bank message or pasted text it came from ("" for an image)
}

// ChooseTxnCategory picks one of categories (or Others) for a captured
// transaction with the Decisions API, from the message and what was read
// from it. ok is false when it couldn't answer; conf is the pick's
// calibrated probability.
func (s *Service) ChooseTxnCategory(ctx context.Context, f TxnFacts, categories []string) (string, float64, bool) {
	kind := f.Type
	if kind != "income" {
		kind = "expense"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s of %s", map[string]string{"expense": "Expense", "income": "Income"}[kind], strconv.FormatFloat(f.Amount, 'f', -1, 64))
	if d := strings.TrimSpace(f.Description); d != "" {
		b.WriteString(" · Paid to/from: " + d)
	}
	if n := strings.TrimSpace(f.Note); n != "" {
		b.WriteString(" · Note: " + n)
	}
	if m := strings.TrimSpace(f.Message); m != "" {
		if len(m) > 1500 {
			m = m[:1500]
		}
		b.WriteString("\nMessage it came from:\n" + m)
	}
	return s.chooseCategory(ctx, b.String(), kind, categories)
}

// chooseCategory picks one of categories (plus Others) for a transaction
// described by input, with the Decisions API. ok is false when it couldn't
// answer, so the caller falls back.
func (s *Service) chooseCategory(ctx context.Context, input, kind string, categories []string) (string, float64, bool) {
	options := map[string]string{}
	canon := map[string]string{}
	for _, c := range append(append([]string(nil), categories...), "Others") {
		c = strings.TrimSpace(c)
		key := strings.ToLower(c)
		if c == "" || len(c) > 128 || canon[key] != "" {
			continue
		}
		canon[key] = c
		options[c] = ""
	}
	whose := "spending"
	if kind == "income" {
		whose = "income"
	}
	instr := "Which of the user's own " + whose + " categories does this transaction belong to? Use Others only if nothing fits."
	choice, conf, err := s.choose(ctx, "categorize", input, instr, options)
	if err != nil {
		// Including a budget refusal: the fallback asks the same gate and
		// returns its error to the caller without calling Gemini.
		return "", 0, false
	}
	if c, ok := canon[strings.ToLower(choice)]; ok {
		return c, conf, true
	}
	return "", 0, false
}

func (s *Service) postDecision(ctx context.Context, body []byte) (string, float64, usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.decisionsURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", s.apiKey)
	resp, err := decisionHTTP.Do(req)
	if err != nil {
		return "", 0, usage{}, fmt.Errorf("decisions api: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", 0, usage{}, fmt.Errorf("decisions api: error %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Answers map[string]struct {
			Choice     string  `json:"choice"`
			Confidence float64 `json:"confidence"`
		} `json:"answers"`
		Usage struct {
			TotalInputTokens int `json:"total_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, usage{}, fmt.Errorf("decisions api: %w", err)
	}
	u := usage{Prompt: out.Usage.TotalInputTokens}
	a, ok := out.Answers["q"]
	if !ok || a.Choice == "" {
		return "", 0, u, errors.New("decisions api: no answer")
	}
	return a.Choice, a.Confidence, u, nil
}

// NewDecisionsOnly is a Service that can only call the Decisions API at
// url: for other packages' tests.
func NewDecisionsOnly(apiKey, url string) *Service {
	return &Service{model: "test", apiKey: apiKey, decisionsURL: url}
}
