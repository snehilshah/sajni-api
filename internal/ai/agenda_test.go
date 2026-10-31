package ai

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"sajni/internal/db"
)

func TestNowDatesOnASunday(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	got := strings.Join(nowDates(time.Date(2026, 11, 1, 21, 5, 0, 0, ist)), "\n")
	for _, want := range []string{
		"Now: 2026-11-01 (Sunday) 21:05",
		"Tomorrow: 2026-11-02 (Monday)",
		"This week: 2026-10-26 to 2026-11-01",
		"Next week: 2026-11-02 to 2026-11-08",
		"This month: 2026-11-01 to 2026-11-30",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestUsageCost(t *testing.T) {
	u := usage{Prompt: 1_000_000, Cached: 800_000, Output: 100_000, Thinking: 100_000}
	// 200k fresh × 0.75 + 800k cached × 0.075 + 200k out × 3.75, per million.
	if got, want := roundCost(u.CostUSD()), 0.96; got != want {
		t.Fatalf("cost = %v, want %v", got, want)
	}
	if u.CachePct() != 80 {
		t.Fatalf("cache pct = %d", u.CachePct())
	}
}

// recordModel answers with text and keeps what it was sent.
type recordModel struct {
	mu   sync.Mutex
	sent []*genai.Content
	cfg  *genai.GenerateContentConfig
}

func (m *recordModel) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return nil, nil
}

func (m *recordModel) GenerateContentStream(_ context.Context, _ string, contents []*genai.Content, cfg *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	m.mu.Lock()
	m.sent, m.cfg = contents, cfg
	m.mu.Unlock()
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		yield(&genai.GenerateContentResponse{
			Candidates:    []*genai.Candidate{{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "ok"}}}}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100, CachedContentTokenCount: 60, CandidatesTokenCount: 5},
		}, nil)
	}
}

// Gemini gets the <now> block with the message; the saved chat doesn't,
// and the system prompt is the same on every request.
func TestNowBlockSentButNotSaved(t *testing.T) {
	model := &recordModel{}
	service := &Service{client: model, model: "fake"}
	var history []*genai.Content
	for ev := range service.Chat(context.Background(), ChatRequest{Mode: "chat", Message: "what's due this week?"}) {
		if ev.Type == "done" {
			var raw struct {
				History []*genai.Content `json:"history"`
			}
			if err := json.Unmarshal(ev.Data, &raw); err != nil {
				t.Fatal(err)
			}
			history = raw.History
		}
	}
	first := model.sent[0]
	if len(first.Parts) != 2 || !strings.HasPrefix(first.Parts[0].Text, "<now>") || first.Parts[1].Text != "what's due this week?" {
		t.Fatalf("sent %+v", first.Parts)
	}
	if model.cfg.SystemInstruction.Parts[0].Text != systemPrompt {
		t.Fatal("system prompt changed per request")
	}
	if len(history) != 2 || len(history[0].Parts) != 1 || history[0].Parts[0].Text != "what's due this week?" {
		t.Fatalf("saved history %+v", history)
	}
}

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/ai -run ListTasks
func TestListTasksOpenByDeadline(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	uid := "01900000-0000-7000-8000-0000000000a1"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'agenda@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var list int64
	d.QueryRow(`INSERT INTO task_lists (user_id, name) VALUES ($1, 'Work') RETURNING id`, uid).Scan(&list)
	add := func(title, status, priority string, due, week, month any) int64 {
		var id int64
		if err := d.QueryRow(`INSERT INTO tasks (user_id, title, status, priority, due_date, week_of, month_of, list_id, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8, NOW() - INTERVAL '3 days') RETURNING id`,
			uid, title, status, priority, due, week, month, list).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// The week of Mon 2030-03-04 to Sun 2030-03-10.
	low := add("low day", "todo", "low", "2030-03-05", nil, nil)
	high := add("high day", "in_progress", "high", "2030-03-08", nil, nil)
	blocked := add("blocked day", "blocked", "medium", "2030-03-06", nil, nil)
	week := add("week task", "todo", "medium", nil, "2030-03-04", nil)
	add("done day", "done", "high", "2030-03-05", nil, nil)
	add("scratched day", "scratched", "high", "2030-03-05", nil, nil)
	month := add("month goal", "todo", "medium", nil, nil, "2030-03-01")
	add("next week", "todo", "high", "2030-03-11", nil, nil)
	d.Exec(`INSERT INTO task_due_history (user_id, task_id, due_date) VALUES ($1,$2,'2030-02-26'),($1,$2,'2030-03-01')`, uid, low)

	ids := func(args map[string]any) ([]int64, []map[string]any) {
		res, _, err := listTasksTool(ctx, d, uid, args)
		if err != nil {
			t.Fatal(err)
		}
		items := res.(map[string]any)["items"].([]map[string]any)
		out := make([]int64, len(items))
		for i, it := range items {
			out[i] = it["id"].(int64)
		}
		return out, items
	}
	got, items := ids(map[string]any{"status": "open", "due_from": "2030-03-04", "due_to": "2030-03-10"})
	// High priority first, then medium by deadline, then low.
	want := []int64{high, blocked, week, low}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
	for _, it := range items {
		switch it["id"].(int64) {
		case week:
			if it["due_by"] != "2030-03-10" {
				t.Errorf("week task due_by = %v", it["due_by"])
			}
		case low:
			if it["missed_count"] != 2 || it["list_name"] != "Work" || it["open_days"] != 3 {
				t.Errorf("low row = %v", it)
			}
		}
	}
	// The month goal's deadline is the 31st.
	if got, _ := ids(map[string]any{"status": "open", "due_from": "2030-03-25", "due_to": "2030-03-31"}); len(got) != 1 || got[0] != month {
		t.Fatalf("month range = %v, want [%d]", got, month)
	}
}
