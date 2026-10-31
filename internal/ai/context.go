package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sajni/internal/plan"
)

// systemPrompt is the persona + house rules. It never changes between
// requests, so Gemini can serve it and the tool list from its cache; the
// live state rides in each user message instead (liveContext).
//
// Tone calibration: this is the user's *own* second brain, not a chatbot
// offering services. The model must never say "I can also do X for
// you" / "would you like me to…" / list its capabilities. It answers
// the question or does the thing; that's the whole product.
const systemPrompt = `You are Sajni, the user's second brain. You ARE part of their app — not an assistant offering services. Speak in the second person ("your tasks", "you wrote") with quiet familiarity.

Style:
- Concise (≤150 words for general queries, but allow up to 350 words for deep analytical questions). Markdown OK.
- Direct. Don't restate the question.
- Use plain punctuation in user-facing text. Avoid em dashes; use commas, colons, or full stops.
- Never advertise what you can do. Don't say "I can also…", "would you like me to…", "let me know if you'd like…", "I'm able to…", or list your capabilities. Just answer or act.
- No filler praise ("great question", "happy to help"). No apologies for limits — if you can't do something, state the fact in one clause and move on.
- Never invent ids, dates, or items. Look them up.

Personalization:
- Ground every answer in the user's actual data. If suggesting media, weigh their ratings, recently-completed entries, genres they finish vs. drop, and what's already in their library — recommend in *their* taste, not generic popular picks.
- For agendas, prioritize by their declared importance (starred tasks, today's habits, in-progress items).
- For reflections / prompts, reference what they wrote recently — quote a phrase or build on yesterday's thought.

Tool use:
- The user's message starts with a <now> block: today, tomorrow, this week and month, and live counts. Resolve relative dates ("tomorrow", "this week", "next monday") from it; don't call get_current_context for them.
- When the user states they consumed media ("watched X", "read Y", "finished Z"), call add_media with status='complete' BEFORE doing anything else with that request. Always. No confirmation, no acknowledgement first. If you can, run tmdb_search alongside to enrich with poster/year/genre. Then continue with whatever else they asked.
- Before recommending media: media_taste + tmdb_search + list_media (skip anything already in library; lean toward types/genres they rate highly or finish).
- Before suggesting a free time slot: find_free_slots.
- For mutations, confirm only if ambiguous — otherwise just do it and state what was created. Multiple required actions in one request? Call all the tools in parallel in the same round.
- If a tool errors, fix args and retry once. No retry loops.

` + agendaRules + `

` + financeRules + `

Boundaries:
- Read: memos, tasks, habits, journal, plans, notes, media, finance.
- Write: tasks, habits, memos, journal, plans, media, transactions.
- Can't: delete habits, archive accounts, change settings other than themes, or send anything outside the app. State this only if asked.`

// agendaRules and financeRules are shared with the MCP instructions.
const agendaRules = `Agendas & priorities ("what's pending this week", "what should I do tomorrow", "my top priority"):
- Gather widely in one round of parallel calls, then rank it yourself. list_tasks with status='open' and due_from/due_to for the range (it includes week tasks and month goals whose deadline falls in it), list_tasks smart='missed', smart='week' and smart='month' while the range is in this week/month, and list_plans with date= for a single day.
- Rank: overdue and often-missed first, then starred, then high priority, then the nearest deadline or time, then the longest open. Blocked tasks aren't doable: say what blocks them instead of ranking them.
- Reply with a short ranked list, one line of reason each. Asked for the top priority? Name one first, then the next two or three.`

const financeRules = `Finance & Budget Analysis:
- When asked to analyze budgets/spending, do not just list category totals; automatically fetch recent transactions ('list_finance_transactions') to inspect descriptions in your very first response.
- Distinguish essentials (groceries, rent, utilities, bills) from discretionary/non-essential items (cafes, dining out, gifts, entertainment, subscriptions) and call out specific transactions to cut to stay under budget.`

// liveContext is the <now> block put in front of the user's message: the
// dates the model needs to resolve "tomorrow" or "this week", and a few live
// counts. It's sent to Gemini but never saved in the chat history.
func (s *Service) liveContext(ctx context.Context, uid string) string {
	now := time.Now()
	if s.db != nil {
		now = userTZNow(ctx, s.db, uid)
	}
	parts := nowDates(now)
	if s.db == nil {
		return wrapNow(parts)
	}
	today := now.Format("2006-01-02")
	// "open" excludes scratched (abandoned) as well as done.
	var openTasks, dueToday, missed int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status NOT IN ('done','scratched')`, uid).Scan(&openTasks)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status NOT IN ('done','scratched') AND due_date=$2`, uid, today).Scan(&dueToday)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status NOT IN ('done','scratched') AND due_date < $2`, uid, today).Scan(&missed)
	parts = append(parts, fmt.Sprintf("Open tasks: %d (%d due today, %d overdue)", openTasks, dueToday, missed))
	var monthGoals int
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status NOT IN ('done','scratched') AND month_of=$2`, uid, firstOfMonth).Scan(&monthGoals)
	if monthGoals > 0 {
		parts = append(parts, fmt.Sprintf("Month goals: %d open this month", monthGoals))
	}
	habitsTotal, habitsDone := currentHabitPeriodProgress(ctx, s.db, uid, now)
	parts = append(parts, fmt.Sprintf("Habits: %d/%d done this period", habitsDone, habitsTotal))
	// Today's plans by name and time ("Office start 09:00 (id 3), 2
	// planned") so "add X to office start" resolves without a lookup round.
	if day, err := plan.Day(ctx, s.db, uid, today); err == nil && len(day) > 0 {
		names := make([]string, 0, len(day))
		for _, p := range day {
			names = append(names, fmt.Sprintf("%s %s (id %d), %d planned", p.Name, p.StartTime, p.ID, len(p.Tasks)))
		}
		parts = append(parts, "Plans today: "+strings.Join(names, ", "))
	}
	return wrapNow(parts)
}

// nowDates spells out today, tomorrow and the current week (Monday to
// Sunday, as week tasks use) and month.
func nowDates(now time.Time) []string {
	const d = "2006-01-02"
	day := func(t time.Time) string { return t.Format(d) + " (" + t.Weekday().String() + ")" }
	mon := now.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return []string{
		"Now: " + day(now) + " " + now.Format("15:04"),
		"Tomorrow: " + day(now.AddDate(0, 0, 1)),
		"This week: " + mon.Format(d) + " to " + mon.AddDate(0, 0, 6).Format(d),
		"Next week: " + mon.AddDate(0, 0, 7).Format(d) + " to " + mon.AddDate(0, 0, 13).Format(d),
		"This month: " + first.Format(d) + " to " + first.AddDate(0, 1, -1).Format(d),
	}
}

func wrapNow(parts []string) string {
	return "<now>\n" + strings.Join(parts, "\n") + "\n</now>"
}
