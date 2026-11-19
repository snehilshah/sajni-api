package ai

import (
	"regexp"
	"sort"
	"strings"

	"google.golang.org/genai"
)

// In the app, Gemini doesn't get all ~120 tool declarations with every
// question: it gets the core ones (everyday reads and writes) plus the groups
// the question is about. A group joins when the question or the chat so far
// mentions it (routeGroups), when an earlier answer in the chat used one of
// its tools, or when the model asks for it with more_tools. Fewer
// declarations cost less per round and pick better. MCP clients see every
// tool (or the toolsets the user granted), so this is in-app only.

// toolGroup maps every non-core tool to its group. A tool missing here is
// core; TestEveryToolHasAGroup keeps the list honest.
var toolGroup = map[string]string{
	// tasks
	"delete_task": "tasks", "scratch_task": "tasks", "block_task": "tasks", "set_task_important": "tasks",
	"add_task_reminder": "tasks", "skip_task_occurrence": "tasks", "create_task_list": "tasks",
	"add_task_to_plan": "tasks", "save_plan": "tasks", "delete_plan": "tasks",
	// reminders
	"update_reminder": "reminders", "delete_reminder": "reminders", "snooze_reminder": "reminders",
	"skip_reminder_occurrence": "reminders", "delete_reminder_history": "reminders",
	// money
	"delete_transaction": "money", "finance_month": "money",
	"list_finance_budgets": "money", "create_budget": "money", "list_slates": "money", "create_slate": "money",
	"update_slate": "money", "move_transactions_to_slate": "money", "list_captures": "money",
	"set_capture_tracking": "money", "list_billers": "money", "create_biller": "money", "pay_biller": "money",
	"unlink_biller_payment": "money", "dismiss_recurring_suggestion": "money",
	"create_finance_account": "money", "update_finance_account": "money",
	// people: lends and money owed, both ways
	"list_lends": "people", "mark_paid_for": "people", "set_split_share": "people", "forgive_lend": "people",
	"settle_lends": "people", "set_payment_toward": "people", "edit_adjustment": "people",
	"list_money_owed": "people", "borrow": "people", "pay_back": "people", "net_off": "people",
	"undo_owed": "people", "lend_before_sajni": "people",
	// investments
	"list_investments": "investments", "create_investment": "investments", "update_investment": "investments",
	"set_investment_auto_debit": "investments",
	// library
	"list_media": "library", "media_taste": "library", "tmdb_search": "library", "list_bookmarks": "library",
	"create_bookmark": "library", "update_bookmark": "library",
	// notes
	"create_note": "notes", "create_folder": "notes", "pin_note": "notes", "pin_folder": "notes",
	"time_travel": "notes", "list_insights": "notes",
	// habits
	"create_habit": "habits",
	// events (things the user tracks with values)
	"list_events": "events", "list_event_entries": "events", "create_event": "events", "update_event": "events",
	"delete_event": "events", "add_event_variable": "events", "update_event_variable": "events",
	"delete_event_variable": "events", "log_event": "events", "update_event_entry": "events",
	"delete_event_entry": "events",
	// projects
	"list_thinking_projects": "projects", "get_thinking_project": "projects", "create_thinking_project": "projects",
	"add_thought": "projects", "comment_on_thought": "projects", "update_thought_comment": "projects",
	"delete_thought_comment": "projects", "set_thought_state": "projects",
	// app
	"reroll_avatar": "app", "generate_theme": "app", "list_themes": "app", "activate_theme": "app",
}

// groups, in the order their tools are declared. words catch a question
// that needs the group; about tells the model what more_tools would add.
var groups = []struct {
	name, about string
	words       *regexp.Regexp
}{
	{"tasks", "delete, scratch, block or star tasks, task reminders, skip a repeat, new task lists, plans (save, delete, add a task)",
		regexp.MustCompile(`(?i)\b(delete|remove|scratch|drop|block|blocked|star|starred|important|plan|plans|planning|skip|new list|task list|subtask|cancel)`)},
	{"reminders", "edit, snooze, skip or delete reminders",
		regexp.MustCompile(`(?i)\b(remind|reminder|reminders|snooze|nudge)`)},
	{"money", "edit or delete transactions, month analytics, budgets, slates, captured bank messages, recurring items (billers), accounts",
		regexp.MustCompile(`(?i)(₹|\brs\.?\s*\d|\b(budget|slate|capture|captured|biller|bill|bills|account|card|spend|spent|spending|expense|income|salary|transfer|refund|categor|transaction|money|finance|paid|pay|emi|sms|trip|cost|costs|saving|savings|expensive|subscription|subscriptions|recurring|repeats|renewal|renew|netflix|rent))|month.{0,25}\b(high|much|expensive|over)\b`)},
	{"people", "lends, splits, settling up, money owed both ways",
		regexp.MustCompile(`(?i)\b(lend|lent|owe|owes|owed|borrow|borrowed|settle|settled|paid .{0,12}back|pay .{0,12}back|gave .{0,12}back|gave me|sent me|received from|got .{0,10}from|returned|split|splitwise|forgive|adjust|friend|people)`)},
	{"investments", "investments: RDs, FDs, SIPs, auto-debit",
		regexp.MustCompile(`(?i)\b(invest|investment|investments|sip|rd|fd|ppf|nps|mf|deposit|mutual|stock|stocks|share|shares|redeem|redeemed|maturity|matures|matured)`)},
	{"library", "movies, shows, books (list, taste, TMDB) and saved bookmarks",
		regexp.MustCompile(`(?i)\b(movie|movies|film|films|shows|series|tv|book|books|novel|watch|watched|watching|bookmark|bookmarks|article|articles|read later|video|videos|site|link|recommend|tmdb|anime)`)},
	{"notes", "new notes and folders, pins, time travel (what you wrote on a past day), insights",
		regexp.MustCompile(`(?i)\b(note|notes|folder|pin|pinned|unpin|last year|on this day|time travel|insight|insights|pattern|patterns)`)},
	{"habits", "create habits",
		regexp.MustCompile(`(?i)\b(habit|habits|streak|routine)`)},
	{"events", "tracked events: create, log values, edit entries",
		regexp.MustCompile(`(?i)\b(event|events|track|tracking|tracker|weight|log my|measure|reading of)`)},
	{"projects", "Projects: thinking projects, thoughts, comments",
		regexp.MustCompile(`(?i)\b(project|projects|thought|thoughts|thinking|hypothesis|idea|ideas|brainstorm)`)},
	{"app", "themes and the avatar",
		regexp.MustCompile(`(?i)\b(theme|themes|avatar|colou?rs?|palette)`)},
}

// groupOf is a tool's group; "" means core.
func groupOf(tool string) string { return toolGroup[tool] }

// coreFollows: after a core tool, its group stays loaded for the next
// questions, so "make it 300" after a new transaction can edit or delete it.
var coreFollows = map[string]string{
	"create_transaction": "money", "update_transaction": "money", "list_finance_transactions": "money",
	"create_task": "tasks", "update_task": "tasks", "complete_task": "tasks", "reschedule_task": "tasks", "list_tasks": "tasks",
	"create_reminder": "reminders", "list_reminders": "reminders",
	"add_media": "library", "log_habit": "habits", "list_habits": "habits",
}

// routeGroups picks the groups a question needs: from its words and the last
// few user messages, and from the tools the chat has used.
func routeGroups(message string, history []*genai.Content) map[string]bool {
	on := map[string]bool{}
	texts := []string{message}
	seenUser := 0
	for i := len(history) - 1; i >= 0; i-- {
		c := history[i]
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				g := groupOf(p.FunctionCall.Name)
				if g == "" {
					g = coreFollows[p.FunctionCall.Name]
				}
				if g != "" {
					on[g] = true
				}
			}
			if c.Role == "user" && p.Text != "" && seenUser < 2 {
				texts = append(texts, p.Text)
			}
		}
		if c.Role == "user" && len(c.Parts) > 0 && c.Parts[0] != nil && c.Parts[0].Text != "" {
			seenUser++
		}
	}
	joined := strings.Join(texts, "\n")
	for _, g := range groups {
		if g.words.MatchString(joined) {
			on[g.name] = true
		}
	}
	return on
}

// declsFor is the core tools plus the given groups, in registry order, so
// the same set of groups always gives the same declarations (and cache).
func (s *Service) declsFor(on map[string]bool) []*genai.FunctionDeclaration {
	decls := []*genai.FunctionDeclaration{moreToolsDecl()}
	for _, t := range s.tools {
		if g := groupOf(t.Name); g != "" && !on[g] {
			continue
		}
		decls = append(decls, &genai.FunctionDeclaration{Name: t.Name, Description: t.Description, Parameters: t.Schema})
	}
	return decls
}

const moreToolsName = "more_tools"

func moreToolsDecl() *genai.FunctionDeclaration {
	names := make([]string, len(groups))
	var about strings.Builder
	for i, g := range groups {
		names[i] = g.name
		about.WriteString("; " + g.name + ": " + g.about)
	}
	return &genai.FunctionDeclaration{
		Name: moreToolsName,
		Description: "Load more tools when none of yours fits the request. Groups" + about.String() +
			". The tools are yours from the next step on.",
		Parameters: obj(map[string]*genai.Schema{
			"groups": arrayOf(strEnum("", names...), "Groups to load."),
		}, "groups"),
	}
}

// loadGroups adds the groups a more_tools call asked for and says which
// tools arrived.
func (s *Service) loadGroups(on map[string]bool, args map[string]any) map[string]any {
	var added []string
	for _, g := range argStrSlice(args, "groups") {
		known := false
		for _, x := range groups {
			known = known || x.name == g
		}
		if known && !on[g] {
			on[g] = true
			added = append(added, g)
		}
	}
	var tools []string
	for _, t := range s.tools {
		for _, g := range added {
			if groupOf(t.Name) == g {
				tools = append(tools, t.Name)
			}
		}
	}
	sort.Strings(tools)
	return map[string]any{"loaded": added, "tools": tools}
}

// groupNames lists the active groups for the log.
func groupNames(on map[string]bool) []string {
	out := make([]string, 0, len(on))
	for g, ok := range on {
		if ok {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}
