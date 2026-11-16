package ai

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/genai"
)

// schemaRefusingModel rejects any response schema, as Gemini would a bad one.
type schemaRefusingModel struct{ calls atomic.Int32 }

func (m *schemaRefusingModel) GenerateContent(_ context.Context, _ string, _ []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	m.calls.Add(1)
	if cfg.ResponseSchema != nil {
		return nil, errors.New("Error 400, Message: schema, Status: INVALID_ARGUMENT")
	}
	return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{{
		Text: `{"amount": 240, "type": "expense", "description": "Swiggy", "note": "", "date": "2026-11-16", "time": "", "account_hint": "2851", "ref_id": ""}`,
	}}}}}}, nil
}

func (m *schemaRefusingModel) GenerateContentStream(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(func(*genai.GenerateContentResponse, error) bool) {}
}

func TestParseFallsBackWhenTheSchemaIsRefused(t *testing.T) {
	m := &schemaRefusingModel{}
	s := &Service{client: m, model: "fake"}
	p, _, err := s.ParseTransactionMessage(context.Background(), "Rs 240 spent at Swiggy", "2026-11-16", true, &TxnCategories{Expense: []string{"Food"}})
	if err != nil || p.Amount != 240 || p.Description != "Swiggy" || m.calls.Load() != 2 {
		t.Fatalf("parse = %+v, %v after %d calls", p, err, m.calls.Load())
	}
}

// emptyThenAnswer returns an empty round, then answers; it records whether
// the retry had every group's tools.
type emptyThenAnswer struct {
	calls    atomic.Int32
	retryHad bool
	firstHad bool
}

func (m *emptyThenAnswer) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return nil, nil
}

func (m *emptyThenAnswer) GenerateContentStream(_ context.Context, _ string, _ []*genai.Content, cfg *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	n := m.calls.Add(1)
	has := false
	for _, d := range cfg.Tools[0].FunctionDeclarations {
		has = has || d.Name == "list_themes"
	}
	if n == 1 {
		m.firstHad = has
	} else {
		m.retryHad = has
	}
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		if n == 1 {
			yield(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{FinishReason: "MALFORMED_FUNCTION_CALL"}}}, nil)
			return
		}
		yield(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{
			Role: "model", Parts: []*genai.Part{{Text: "ok"}},
		}}}}, nil)
	}
}

func TestEmptyRoundRetriesWithEveryTool(t *testing.T) {
	m := &emptyThenAnswer{}
	s := &Service{client: m, model: "fake", tools: []Tool{{Name: "list_themes"}}}
	var events []string
	for ev := range s.Chat(context.Background(), ChatRequest{Mode: "palette", Message: "anything?"}) {
		events = append(events, ev.Type)
	}
	if m.firstHad || !m.retryHad || !strings.Contains(strings.Join(events, " "), "done") {
		t.Fatalf("first had app tools %v, retry %v, events %v", m.firstHad, m.retryHad, events)
	}
}

// alwaysEmpty never answers: the turn ends in an error, not a blank "done".
type alwaysEmpty struct{}

func (alwaysEmpty) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return nil, nil
}

func (alwaysEmpty) GenerateContentStream(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(func(*genai.GenerateContentResponse, error) bool) {}
}

func TestBlankAnswerIsAnError(t *testing.T) {
	s := &Service{client: alwaysEmpty{}, model: "fake"}
	last := ""
	for ev := range s.Chat(context.Background(), ChatRequest{Mode: "chat", Message: "hm"}) {
		last = ev.Type + string(ev.Data)
	}
	if !strings.HasPrefix(last, "error") || !strings.Contains(last, "history") {
		t.Fatalf("last event %s", last)
	}
}
