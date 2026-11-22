package ai

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/genai"
)

// textModel answers every one-shot call with text and counts the calls.
type textModel struct {
	text  string
	calls int
}

func (m *textModel) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	m.calls++
	return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{{Text: m.text}}}}}}, nil
}

func (m *textModel) GenerateContentStream(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(func(*genai.GenerateContentResponse, error) bool) {}
}

// Categorise asks the Decisions API first and maps its choice back to the
// user's own spelling; when it fails, the generative call answers instead.
func TestCategorizeUsesDecisionsThenFallsBack(t *testing.T) {
	var got struct {
		Model     string `json:"model"`
		Input     string `json:"input"`
		Questions map[string]struct {
			Type     string            `json:"type"`
			Criteria map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			w.WriteHeader(401)
			return
		}
		if fail {
			w.WriteHeader(503)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"answers":{"q":{"type":"choice","choice":"food & dining","confidence":0.83,"probabilities":{}}},"usage":{"total_input_tokens":176}}`))
	}))
	defer srv.Close()
	model := &textModel{text: "Transport"}
	s := &Service{client: model, model: "gemini-4-hydrogen", apiKey: "k", decisionsURL: srv.URL}
	cats := []string{"Food & Dining", "Transport", "Others"}

	picked, cost, err := s.CategorizeExpense(context.Background(), "Pahunchar Fastfood", "expense", cats)
	if err != nil || picked != "Food & Dining" || cost == 0 {
		t.Fatalf("decisions: %q %d %v", picked, cost, err)
	}
	if model.calls != 0 {
		t.Fatalf("generative call made too: %d", model.calls)
	}
	q := got.Questions["q"]
	if got.Model != decisionModel || q.Type != "choice" || len(q.Criteria) != 3 || got.Input == "" {
		t.Fatalf("request: %+v", got)
	}

	fail = true
	picked, _, err = s.CategorizeExpense(context.Background(), "Uber India", "expense", cats)
	if err != nil || picked != "Transport" || model.calls != 1 {
		t.Fatalf("fallback: %q %v calls=%d", picked, err, model.calls)
	}
}

func TestPriceOf(t *testing.T) {
	nov := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	jan := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	u := usage{Prompt: 1_000_000, Cached: 0, Output: 1_000_000}
	for _, tc := range []struct {
		model string
		at    time.Time
		want  float64
	}{
		{"gemini-4-hydrogen", nov, 9.00},
		{"gemini-3.8-flash", nov, 4.50},
		{"gemini-3.8-flash", jan, 9.00},
		{decisionModel, nov, 0.10},
	} {
		if got := roundCost(u.CostUSD(tc.model, tc.at)); got != tc.want {
			t.Errorf("%s at %s: %v, want %v", tc.model, tc.at.Format("2006-01"), got, tc.want)
		}
	}
	if got := scaleParts(map[string]int{"tools": 300, "message": 100}, 1000); got["tools"] != 750 || got["message"] != 250 {
		t.Fatalf("scaleParts: %v", got)
	}
}
