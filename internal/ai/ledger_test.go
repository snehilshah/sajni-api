package ai

import (
	"context"
	"encoding/json"
	"iter"
	"testing"
	"time"

	"google.golang.org/genai"

	"sajni/internal/db"
	"sajni/internal/dbtest"
)

// toolThenTextModel calls "read" once, then answers; each round reports usage.
type toolThenTextModel struct{ n int }

func (m *toolThenTextModel) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return nil, nil
}

func (m *toolThenTextModel) GenerateContentStream(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	m.n++
	first := m.n == 1
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		part := &genai.Part{Text: "done"}
		if first {
			part = &genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "read", Args: map[string]any{}}}
		}
		yield(&genai.GenerateContentResponse{
			Candidates:    []*genai.Candidate{{Content: &genai.Content{Role: "model", Parts: []*genai.Part{part}}, FinishReason: "STOP"}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 4000, CachedContentTokenCount: 1000, CandidatesTokenCount: 20, ThoughtsTokenCount: 50},
		}, nil)
	}
}

// Needs a scratch Postgres. Every round of a chat question lands in the
// ledger with its model, turn, prompt parts and the tools it called.
func TestLedgerRecordsChatRounds(t *testing.T) {
	d, err := db.New(dbtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000a9"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, $2, 'T')`, uid, uid+"@test"); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)

	s := &Service{db: d, client: &toolThenTextModel{}, model: "gemini-4-hydrogen",
		modelByKind: map[string]string{"palette": "gemini-3.8-flash"},
		tools: []Tool{{Name: "read", Handler: func(context.Context, string, map[string]any) (any, map[string]any, error) {
			return map[string]any{"rows": "a long enough result to count"}, nil, nil
		}}}}
	for range s.Chat(context.Background(), ChatRequest{UserID: uid, Mode: "chat", Message: "what's due?"}) {
	}

	type row struct {
		Model, Thinking, Turn string
		Round, In             int
		Cost                  float64
		Parts, Results        []byte
		Calls                 []string
		OK                    bool
	}
	var rows []row
	for i := 0; i < 40 && len(rows) < 2; i++ { // written off the request path
		time.Sleep(50 * time.Millisecond)
		rows = rows[:0]
		rs, err := d.Query(`SELECT model, thinking, turn_id, round, in_tokens, cost_usd, COALESCE(parts::text,'{}'), COALESCE(results::text,'{}'), calls, ok
			FROM ai_calls WHERE user_id=$1 AND kind='chat' ORDER BY round`, uid)
		if err != nil {
			t.Fatal(err)
		}
		for rs.Next() {
			var r row
			var parts, results string
			var calls []string
			if err := rs.Scan(&r.Model, &r.Thinking, &r.Turn, &r.Round, &r.In, &r.Cost, &parts, &results, &calls, &r.OK); err != nil {
				t.Fatal(err)
			}
			r.Parts, r.Results, r.Calls = []byte(parts), []byte(results), calls
			rows = append(rows, r)
		}
		rs.Close()
	}
	if len(rows) != 2 {
		t.Fatalf("%d ledger rows, want 2", len(rows))
	}
	first := rows[0]
	if first.Model != "gemini-4-hydrogen" || first.Thinking != "medium" || first.Turn == "" || first.Turn != rows[1].Turn || !first.OK {
		t.Fatalf("round 1: %+v", first)
	}
	// 3000 fresh × 1.50 + 1000 cached × 0.075 + 70 out × 7.50, per million.
	if first.Cost != roundCost((3000*1.50+1000*0.075+70*7.50)/1e6) {
		t.Fatalf("cost %v", first.Cost)
	}
	var parts map[string]int
	json.Unmarshal(first.Parts, &parts)
	sum := 0
	for _, n := range parts {
		sum += n
	}
	if parts["system"] == 0 || parts["message"] == 0 || sum < 3990 || sum > 4010 {
		t.Fatalf("parts %v (sum %d)", parts, sum)
	}
	var results map[string]int
	json.Unmarshal(first.Results, &results)
	if len(first.Calls) != 1 || first.Calls[0] != "read" || results["read"] == 0 {
		t.Fatalf("calls %v results %v", first.Calls, results)
	}
	if parts2 := string(rows[1].Parts); rows[1].Round != 2 || len(parts2) < 3 {
		t.Fatalf("round 2: %+v", rows[1])
	}
	if s.modelFor("palette") != "gemini-3.8-flash" || s.modelFor("parse_text") != "gemini-4-hydrogen" {
		t.Fatal("modelFor")
	}
}
