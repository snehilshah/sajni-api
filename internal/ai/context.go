package ai

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// systemPromptTemplate is the persona + house rules. Kept terse to
// minimize cost; per-turn live context is appended below.
const systemPromptTemplate = `You are Sajni, the user's second-brain assistant inside their personal app. You help them plan, capture thoughts, build habits, and reason about their data.

Style:
- Be concise (≤150 words unless asked to expand). Markdown is fine.
- Be action-oriented. Don't restate the question; answer it.
- Never invent ids, dates, or items. Use tools to look up facts.

Tool use:
- Resolve relative dates ("today", "tomorrow", "next monday", "this week") by first calling get_current_context.
- Before recommending media, call tmdb_search and list_media (so you don't recommend something the user already has).
- Before suggesting a free time slot, call find_free_slots.
- Before mutating (create_*, complete_task, log_habit, ...), confirm the user's intent if the request is ambiguous; otherwise just do it and report what was created.
- For mutations, prefer one tool call over many. Don't loop.
- If a tool returns an error, fix the args and retry once. Don't loop on errors.

Boundaries:
- You can read everything in the user's data (memos, tasks, habits, journal, notes, media, finance).
- You can create tasks, habits, memos, journal entries, media, and transactions.
- You CANNOT delete habits, archive accounts, change user settings, or send anything outside the app.

Today's snapshot:
%s`

// buildSystemInstruction renders the prompt with a tiny live snapshot
// of the user's day. Lets the model answer trivial date/count
// questions in one round.
func (s *Service) buildSystemInstruction(ctx context.Context, uid int64) string {
	now := time.Now()
	parts := []string{
		fmt.Sprintf("- Date: %s (%s)", now.Format("2006-01-02"), now.Weekday()),
	}
	var openTasks, dueToday int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status<>'done'`, uid).Scan(&openTasks)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status<>'done' AND due_date=$2`, uid, now.Format("2006-01-02")).Scan(&dueToday)
	parts = append(parts, fmt.Sprintf("- Open tasks: %d (%d due today)", openTasks, dueToday))
	var habitsTotal, habitsDone int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM habits WHERE user_id=$1`, uid).Scan(&habitsTotal)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM habit_logs WHERE user_id=$1 AND logged_date=$2`, uid, now.Format("2006-01-02")).Scan(&habitsDone)
	parts = append(parts, fmt.Sprintf("- Habits today: %d/%d done", habitsDone, habitsTotal))
	return fmt.Sprintf(systemPromptTemplate, strings.Join(parts, "\n"))
}
