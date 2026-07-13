package ai

import (
	"context"
	"iter"
	"sync/atomic"
	"testing"

	"google.golang.org/genai"
)

type terminalTurnModel struct {
	calls       atomic.Int32
	noneCalls   atomic.Int32
	toolEnabled atomic.Int32
}

func (m *terminalTurnModel) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return nil, nil
}

func (m *terminalTurnModel) GenerateContentStream(_ context.Context, _ string, _ []*genai.Content, cfg *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		m.calls.Add(1)
		mode := genai.FunctionCallingConfigModeAuto
		if cfg.ToolConfig != nil && cfg.ToolConfig.FunctionCallingConfig != nil {
			mode = cfg.ToolConfig.FunctionCallingConfig.Mode
		}
		part := &genai.Part{}
		if mode == genai.FunctionCallingConfigModeNone {
			m.noneCalls.Add(1)
			part.Text = "done"
		} else {
			m.toolEnabled.Add(1)
			part.FunctionCall = &genai.FunctionCall{Name: "mutate", Args: map[string]any{}}
		}
		yield(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Role: "model", Parts: []*genai.Part{part}}}}}, nil)
	}
}

func TestFinalToolRoundGetsToolDisabledSynthesis(t *testing.T) {
	for _, test := range []struct {
		mode   string
		rounds int32
	}{{"palette", paletteRounds}, {"chat", maxToolRounds}} {
		t.Run(test.mode, func(t *testing.T) {
			model := &terminalTurnModel{}
			var mutations atomic.Int32
			service := &Service{client: model, model: "fake", tools: []Tool{{
				Name: "mutate", Mutating: true,
				Handler: func(context.Context, string, map[string]any) (any, map[string]any, error) {
					mutations.Add(1)
					return map[string]any{"ok": true}, nil, nil
				},
			}}}
			var done bool
			for event := range service.Chat(context.Background(), ChatRequest{Mode: test.mode, Message: "run"}) {
				if event.Type == "done" {
					done = true
				}
			}
			if !done {
				t.Fatal("missing done event")
			}
			if got := mutations.Load(); got != test.rounds {
				t.Fatalf("mutations = %d, want %d", got, test.rounds)
			}
			if got := model.noneCalls.Load(); got != 1 {
				t.Fatalf("tool-disabled calls = %d, want 1", got)
			}
			if got := model.toolEnabled.Load(); got != test.rounds {
				t.Fatalf("tool-enabled calls = %d, want %d", got, test.rounds)
			}
		})
	}
}
