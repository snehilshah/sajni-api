package ai

import (
	"context"
	"fmt"
)

// The same tools, offered to outside assistants (ChatGPT, Gemini) through
// the MCP server in internal/mcp. They have no <now> block, so
// get_current_context is how they learn the date.

// appOnlyTools change Sajni's own look; they make no sense from outside.
var appOnlyTools = map[string]bool{
	"reroll_avatar":  true,
	"generate_theme": true,
	"list_themes":    true,
	"activate_theme": true,
}

const currentContextForMCP = "Today's date, weekday and time in the user's timezone, plus tomorrow, this week (Monday to Sunday), next week and this month as date ranges, and live counts (open tasks, due today, overdue, habits). Call it before anything that depends on the date, such as 'tomorrow', 'this week' or 'overdue'."

// MCPInstructions is the guide an outside assistant gets when it connects.
const MCPInstructions = `Sajni is the user's personal knowledge app: tasks, habits, journal, memos, notes, plans, media (movies, shows, books, bookmarks), finance (accounts, transactions, lends, budgets) and projects. Everything here is the user's own data.

- Call get_current_context first whenever dates matter; resolve "tomorrow", "this week" or "next monday" from its dates, never from your own clock.
- Never invent ids, dates or items. Look them up with the list_ and search tools.
- Amounts are in the user's currency (INR) unless a row says otherwise.
- When the user says they watched, read or finished something, call add_media with status='complete'.
- Before suggesting a free time slot, call find_free_slots.
- For changes, act when the request is clear and say what changed. Ask only when it's ambiguous.

` + agendaRules + `

` + financeRules

// ExternalTools is the registry minus the app-only tools.
func (s *Service) ExternalTools() []Tool {
	out := make([]Tool, 0, len(s.tools))
	for _, t := range s.tools {
		if appOnlyTools[t.Name] {
			continue
		}
		if t.Name == "get_current_context" {
			t.Description = currentContextForMCP
		}
		out = append(out, t)
	}
	return out
}

// CallTool runs one tool for uid, as the in-app chat does, so widgets and
// other devices hear about the change.
func (s *Service) CallTool(ctx context.Context, uid, name string, args map[string]any) (any, error) {
	if appOnlyTools[name] {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	data, _, err := s.dispatch(ctx, uid, name, args)
	return data, err
}
