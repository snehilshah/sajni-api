package ai

import (
	"context"
	"iter"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"
)

// coreTools are sent with every in-app question. A new tool goes here only
// if most questions need it; otherwise give it a group in toolGroup.
var coreTools = []string{
	"get_current_context", "search", "fetch",
	"list_tasks", "create_task", "update_task", "complete_task", "reschedule_task", "list_task_lists",
	"list_habits", "log_habit",
	"list_journal_entries", "get_journal_entry", "create_journal_entry",
	"list_recent_memos", "create_memo",
	"list_reminders", "create_reminder", "list_plans", "find_free_slots", "add_media",
	"list_finance_transactions", "create_transaction", "update_transaction", "list_finance_accounts", "list_finance_categories",
}

func TestEveryToolHasAGroup(t *testing.T) {
	core := map[string]bool{}
	for _, n := range coreTools {
		core[n] = true
	}
	known := map[string]bool{}
	for _, tl := range (&Service{}).buildTools() {
		known[tl.Name] = true
		g, grouped := toolGroup[tl.Name]
		switch {
		case grouped && core[tl.Name]:
			t.Errorf("%s is both core and in group %s", tl.Name, g)
		case !grouped && !core[tl.Name]:
			t.Errorf("%s has no group: add it to toolGroup (groups.go) or coreTools", tl.Name)
		}
	}
	for n := range toolGroup {
		if !known[n] {
			t.Errorf("toolGroup names %s, which isn't a tool", n)
		}
	}
	for _, n := range coreTools {
		if !known[n] {
			t.Errorf("coreTools names %s, which isn't a tool", n)
		}
	}
	names := map[string]bool{}
	for _, g := range groups {
		names[g.name] = true
	}
	for n, g := range toolGroup {
		if !names[g] {
			t.Errorf("%s is in unknown group %s", n, g)
		}
	}
}

// The eval set: everyday questions and the tools Gemini must have to answer
// them without a more_tools round. Add a line when a real question misroutes.
func TestRoutingGoldenPrompts(t *testing.T) {
	s := &Service{}
	s.tools = s.buildTools()
	cases := []struct {
		prompt  string
		history []*genai.Content
		need    []string
	}{
		{prompt: "add buy milk to my tasks for tomorrow", need: []string{"create_task"}},
		{prompt: "what's pending this week?", need: []string{"list_tasks", "list_plans"}},
		{prompt: "delete the task about milk", need: []string{"delete_task", "list_tasks"}},
		{prompt: "star the passport task", need: []string{"set_task_important"}},
		{prompt: "plan my day: add the report task to office start", need: []string{"add_task_to_plan"}},
		{prompt: "remind me to call mum at 7pm", need: []string{"create_reminder"}},
		{prompt: "snooze my medicine reminder by an hour", need: []string{"snooze_reminder"}},
		{prompt: "how much did I spend on food last month?", need: []string{"list_finance_transactions", "finance_month"}},
		{prompt: "₹450 swiggy dinner on HDFC", need: []string{"create_transaction"}},
		{prompt: "am I over budget?", need: []string{"list_finance_budgets"}},
		{prompt: "move the Goa expenses to a slate", need: []string{"move_transactions_to_slate", "list_slates"}},
		{prompt: "Riya paid me back 500", need: []string{"settle_lends", "list_lends"}},
		{prompt: "how much does Amit owe me?", need: []string{"list_lends", "list_money_owed"}},
		{prompt: "I borrowed 2000 from dad", need: []string{"borrow"}},
		{prompt: "my RD matured today", need: []string{"list_investments"}},
		{prompt: "I watched Dune yesterday, loved it", need: []string{"add_media", "tmdb_search"}},
		{prompt: "recommend a movie for tonight", need: []string{"media_taste", "tmdb_search", "list_media"}},
		{prompt: "save this link for later https://example.com", need: []string{"create_bookmark"}},
		{prompt: "what did I write on this day last year?", need: []string{"time_travel"}},
		{prompt: "make a note called Trip ideas", need: []string{"create_note"}},
		{prompt: "start a habit: read 20 pages", need: []string{"create_habit"}},
		{prompt: "log my weight, 72 kg", need: []string{"log_event", "list_events"}},
		{prompt: "add a thought to my startup project", need: []string{"add_thought", "list_thinking_projects"}},
		{prompt: "change my theme to something calm", need: []string{"generate_theme"}},
		{prompt: "add to today's journal: lunch with Riya", need: []string{"create_journal_entry"}},
		{prompt: "read me note 12", need: []string{"fetch"}},
		{prompt: "what did the Goa trip cost?", need: []string{"list_slates"}},
		{prompt: "why is this month so high?", need: []string{"finance_month"}},
		{prompt: "when does my Netflix subscription renew?", need: []string{"list_billers"}},
		{prompt: "Rahul gave me 500", need: []string{"settle_lends"}},
		{prompt: "I redeemed my PPF", need: []string{"list_investments"}},
		{prompt: "save this article to read later", need: []string{"create_bookmark"}},
		{
			// After a core write, its group stays for the follow-up.
			prompt: "make it 300",
			history: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{{Text: "250 chai from HDFC"}}},
				{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "create_transaction"}}}},
			},
			need: []string{"update_transaction", "delete_transaction"},
		},
		{
			// A follow-up keeps the group the chat already used.
			prompt: "and the other one too",
			history: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{{Text: "get rid of the old milk task"}}},
				{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "delete_task"}}}},
			},
			need: []string{"delete_task"},
		},
	}
	for _, c := range cases {
		have := map[string]bool{}
		for _, d := range s.declsFor(routeGroups(c.prompt, c.history)) {
			have[d.Name] = true
		}
		for _, n := range c.need {
			if !have[n] {
				t.Errorf("%q: %s not loaded (groups %v)", c.prompt, n, groupNames(routeGroups(c.prompt, c.history)))
			}
		}
	}
	// A plain question stays small.
	plain := s.declsFor(routeGroups("what's on today?", nil))
	if len(plain) > 35 {
		t.Errorf("a plain question sends %d tools", len(plain))
	}
}

// groupsModel asks for more_tools, then calls the tool it brought in.
type groupsModel struct {
	round    atomic.Int32
	declared [][]string
}

func (m *groupsModel) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return nil, nil
}

func (m *groupsModel) GenerateContentStream(_ context.Context, _ string, _ []*genai.Content, cfg *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	var names []string
	if len(cfg.Tools) > 0 {
		for _, d := range cfg.Tools[0].FunctionDeclarations {
			names = append(names, d.Name)
		}
	}
	sort.Strings(names)
	m.declared = append(m.declared, names)
	n := m.round.Add(1)
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		call := func(name string, args map[string]any) {
			yield(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{
				Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: name, Args: args}}},
			}}}}, nil)
		}
		switch n {
		case 1:
			call(moreToolsName, map[string]any{"groups": []any{"app"}})
		case 2:
			call("list_themes", map[string]any{})
		default:
			yield(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{
				Role: "model", Parts: []*genai.Part{{Text: "done"}},
			}}}}, nil)
		}
	}
}

func TestMoreToolsLoadsAGroupQuietly(t *testing.T) {
	model := &groupsModel{}
	s := &Service{client: model, model: "fake", tools: []Tool{
		{Name: "list_tasks", Handler: func(context.Context, string, map[string]any) (any, map[string]any, error) { return nil, nil, nil }},
		{Name: "list_themes", Handler: func(context.Context, string, map[string]any) (any, map[string]any, error) {
			return []string{"calm"}, nil, nil
		}},
	}}
	var shown []string
	for ev := range s.Chat(context.Background(), ChatRequest{Mode: "palette", Message: "anything new?"}) {
		if ev.Type == "tool_call" {
			shown = append(shown, string(ev.Data))
		}
		if ev.Type == "error" {
			t.Fatal(string(ev.Data))
		}
	}
	if len(model.declared) != 3 {
		t.Fatalf("rounds = %d", len(model.declared))
	}
	if strings.Contains(strings.Join(model.declared[0], ","), "list_themes") {
		t.Fatal("app tools sent before they were asked for")
	}
	if !strings.Contains(strings.Join(model.declared[1], ","), "list_themes") {
		t.Fatal("more_tools didn't load the app group")
	}
	if len(shown) != 1 || !strings.Contains(shown[0], "list_themes") {
		t.Fatalf("tool calls shown to the user: %v", shown)
	}
}

// stallModel stalls on its first stream, then answers.
type stallModel struct{ calls atomic.Int32 }

func (m *stallModel) GenerateContent(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return nil, nil
}

func (m *stallModel) GenerateContentStream(ctx context.Context, _ string, _ []*genai.Content, _ *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	n := m.calls.Add(1)
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		if n == 1 {
			<-ctx.Done()
			yield(nil, ctx.Err())
			return
		}
		yield(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{
			Role: "model", Parts: []*genai.Part{{Text: "here"}},
		}}}}, nil)
	}
}

func TestStalledRoundIsTriedOnce(t *testing.T) {
	model := &stallModel{}
	s := &Service{client: model, model: "fake", roundIdle: 20 * time.Millisecond}
	got := ""
	for ev := range s.Chat(context.Background(), ChatRequest{Mode: "palette", Message: "hi"}) {
		got += ev.Type + " "
	}
	if !strings.Contains(got, "done") {
		t.Fatalf("events: %s", got)
	}
}
