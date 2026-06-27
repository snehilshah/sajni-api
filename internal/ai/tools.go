package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/genai"

	"sajni/internal/db"
	"sajni/internal/reminderqueue"
	"sajni/internal/storage"
)

// Tool wraps a function declaration with its server-side handler. The
// handler always receives the authenticated user id, never trusting any
// userId argument from the model.
//
// Returning (data, meta, err):
//
//	data — JSON-serializable result the model sees as the function's return
//	meta — optional UI hint surfaced via the tool_result event (e.g.
//	       {"kind":"task_created","id":42,"title":"…"}). nil for read tools.
type Tool struct {
	Name        string
	Description string
	Schema      *genai.Schema
	Mutating    bool
	Handler     func(ctx context.Context, uid string, args map[string]any) (data any, meta map[string]any, err error)
}

// dispatch looks up a tool by name and invokes it. Unknown tools and
// bad-arg errors come back as structured errors so the model can recover.
func (s *Service) dispatch(ctx context.Context, uid string, name string, args map[string]any) (any, map[string]any, error) {
	for _, t := range s.tools {
		if t.Name == name {
			return t.Handler(ctx, uid, args)
		}
	}
	return nil, nil, fmt.Errorf("unknown tool: %s", name)
}

// ----- schema helpers -----

func obj(props map[string]*genai.Schema, required ...string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeObject, Properties: props, Required: required}
}
func str(d string) *genai.Schema  { return &genai.Schema{Type: genai.TypeString, Description: d} }
func intg(d string) *genai.Schema { return &genai.Schema{Type: genai.TypeInteger, Description: d} }
func num(d string) *genai.Schema  { return &genai.Schema{Type: genai.TypeNumber, Description: d} }
func boolean(d string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeBoolean, Description: d}
}

func arrayOf(item *genai.Schema, d string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeArray, Items: item, Description: d}
}

// ----- argument helpers -----

func argStr(args map[string]any, k string) string {
	if v, ok := args[k]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argInt(args map[string]any, k string, def int64) int64 {
	if v, ok := args[k]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		}
	}
	return def
}

func argBool(args map[string]any, k string, def bool) bool {
	if v, ok := args[k]; ok && v != nil {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func argFloat(args map[string]any, k string) float64 {
	if v, ok := args[k]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return n
		case int64:
			return float64(n)
		case int:
			return float64(n)
		}
	}
	return 0
}

func argStrSlice(args map[string]any, k string) []string {
	if v, ok := args[k]; ok && v != nil {
		if arr, ok := v.([]any); ok {
			out := make([]string, 0, len(arr))
			for _, x := range arr {
				if s, ok := x.(string); ok && s != "" {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}

// nullableStr returns nil for empty strings so we get NULL in DB instead of "".
func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// userTZLoc resolves the user's captured IANA timezone, falling back to
// Asia/Kolkata (every Sajni user is IST). Cloud Run runs UTC, so deriving a
// "today" or a due-date from a bare time.Now() shifts the day by 5.5h and
// mis-resolves "today"/"tomorrow" for anyone awake past midnight IST. Mirrors
// the api package's userLocation (kept local to avoid an import cycle).
func userTZLoc(ctx context.Context, d *db.DB, uid string) *time.Location {
	var tz string
	d.QueryRowContext(ctx, `SELECT COALESCE(timezone,'') FROM users WHERE id=$1`, uid).Scan(&tz)
	if tz == "" {
		tz = "Asia/Kolkata"
	}
	if l, err := time.LoadLocation(tz); err == nil {
		return l
	}
	return time.UTC
}

// userTZNow is time.Now() in the user's timezone. See userTZLoc.
func userTZNow(ctx context.Context, d *db.DB, uid string) time.Time {
	return time.Now().In(userTZLoc(ctx, d, uid))
}

// buildTools constructs the registry. Captures *Service so tools can
// reach the DB and storage backend.
func (s *Service) buildTools() []Tool {
	d := s.db
	store := s.store
	return []Tool{
		// ---------------- READ ----------------
		{
			Name:        "get_current_context",
			Description: "Returns today's date in ISO format, the current weekday, and a quick summary of the user's day (open task count, habits done/pending, recent memo count). Call this first when the user asks about 'today', 'tomorrow', 'this week', etc.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return getCurrentContextTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_tasks",
			Description: "List the user's tasks with optional filters. Use this before suggesting actions on tasks. Smart filters: 'my_day' (due today), 'important' (starred), 'planned' (has due date), 'week' (week-scoped tasks for the current week), 'month' (month goals for the current month), 'missed' (overdue and still open — past due_date, not done/scratched; a month goal's child sessions are rolled up to the goal, not listed individually), 'inbox' (no list). Scratched (abandoned) tasks are excluded from every smart filter; pass status='scratched' to see them.",
			Schema: obj(map[string]*genai.Schema{
				"smart":     str("Smart-list shortcut: 'my_day' | 'important' | 'planned' | 'week' | 'month' | 'missed' | 'inbox' | 'all'."),
				"list_id":   intg("Filter to a specific user list."),
				"parent_id": intg("Set to non-zero to fetch children of a parent task. Default returns top-level only."),
				"status":    str("Filter by status: 'todo', 'in_progress', 'done', 'scratched'."),
				"due_from":  str("Lower bound (inclusive) ISO date YYYY-MM-DD."),
				"due_to":    str("Upper bound (inclusive) ISO date YYYY-MM-DD."),
				"priority":  str("Filter by priority: 'high', 'medium', 'low'."),
				"important": boolean("Restrict to starred tasks."),
				"limit":     intg("Maximum tasks to return (default 50)."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listTasksTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_task_lists",
			Description: "List the user's custom task lists (groupings) with their open-task counts. Use to disambiguate when a request mentions a list by name (e.g. 'add to my Work list').",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listTaskListsTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_habits",
			Description: "List the user's habits with done-today flag and total log count.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listHabitsTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_journal_entries",
			Description: "List recent journal entry metadata (date, mood). Use get_journal_entry to read content.",
			Schema: obj(map[string]*genai.Schema{
				"date_from": str("Lower bound ISO date."),
				"date_to":   str("Upper bound ISO date."),
				"limit":     intg("Default 20."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listJournalTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "get_journal_entry",
			Description: "Read the full content of one journal entry by date.",
			Schema: obj(map[string]*genai.Schema{
				"date": str("ISO date YYYY-MM-DD."),
			}, "date"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return getJournalTool(ctx, d, store, uid, argStr(args, "date"))
			},
		},
		{
			Name:        "list_recent_memos",
			Description: "Read recent memos. Optional substring filter via 'query'.",
			Schema: obj(map[string]*genai.Schema{
				"limit": intg("Default 20."),
				"query": str("Optional ILIKE substring."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listMemosTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_media",
			Description: "List the user's media library. Use to avoid recommending things they already have.",
			Schema: obj(map[string]*genai.Schema{
				"status": str("'pending', 'watching', 'done', etc."),
				"type":   str("'movie', 'show', 'book'."),
				"limit":  intg("Default 30."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listMediaTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_bookmarks",
			Description: "List the user's saved bookmarks (links to videos, articles, sites). Filter by kind, unread (read-later queue), or a substring query.",
			Schema: obj(map[string]*genai.Schema{
				"kind":   str("'video' | 'site'."),
				"unread": boolean("true = only the read-later queue, false = only already-read."),
				"query":  str("Optional ILIKE substring over title/url/note."),
				"limit":  intg("Default 30."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listBookmarksTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "media_taste",
			Description: "Returns the user's taste profile: top-rated titles, favourite genres (weighted by rating), completion vs. drop ratio per type, and most-recently completed entries. Call this once BEFORE tmdb_search when recommending anything so the suggestion is personalised, not generic.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return mediaTasteTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_finance_accounts",
			Description: "List finance accounts with current computed balance.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listAccountsTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_finance_transactions",
			Description: "List finance transactions with filters. Each transaction includes its category_id and category_name.",
			Schema: obj(map[string]*genai.Schema{
				"account_id": intg("Filter by account."),
				"date_from":  str("ISO date."),
				"date_to":    str("ISO date."),
				"limit":      intg("Default 50."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listTxnsTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_finance_categories",
			Description: "List the user's finance categories (such as 'Food & Dining', 'Rent', 'Utilities', 'Salary', etc.) with their kind ('expense' or 'income').",
			Schema: obj(map[string]*genai.Schema{
				"kind": str("Optional filter: 'expense' | 'income'."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listCategoriesTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_finance_budgets",
			Description: "List the user's budgets with their period, start and end dates, total budget amount, total actual spent, and category breakdown (target vs spent). Use this to see if the user is over budget or analyzing spending.",
			Schema: obj(map[string]*genai.Schema{
				"limit": intg("Maximum budgets to return (default 10)."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listBudgetsTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "search",
			Description: "Free-text search across memos, tasks, notes, journals, habits, media, and transactions. Use for 'find anything about X' style questions.",
			Schema: obj(map[string]*genai.Schema{
				"q":     str("Search query."),
				"types": arrayOf(str(""), "Optional whitelist of types: memo, task, note, journal, habit, media, transaction."),
			}, "q"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return runSearchTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "tmdb_search",
			Description: "Search TMDB for real movie or show metadata (title, year, release_date, release_state, poster, overview). Use this before recommending media so the suggestion comes with real data and an external_id you can pass to add_media.",
			Schema: obj(map[string]*genai.Schema{
				"q":    str("Movie or show title to search."),
				"type": str("'movie' or 'show'. Defaults to movie."),
			}, "q"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return tmdbSearchTool(ctx, argStr(args, "q"), argStr(args, "type"))
			},
		},
		{
			Name:        "find_free_slots",
			Description: "Find available time blocks of the requested duration on a given day, considering tasks with scheduled_at. Respects work hours (default 09:00–21:00).",
			Schema: obj(map[string]*genai.Schema{
				"date":         str("ISO date YYYY-MM-DD."),
				"duration_min": intg("Slot length in minutes. Default 60."),
				"earliest":     str("HH:MM lower bound (default 09:00)."),
				"latest":       str("HH:MM upper bound (default 21:00)."),
			}, "date"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return findFreeSlotsTool(ctx, d, uid, args)
			},
		},

		// ---------------- WRITE ----------------
		{
			Name:        "create_task",
			Description: "Create a new task. Resolve relative dates ('tomorrow', 'next monday') against get_current_context first. Use list_task_lists to look up list_id by name; use list_tasks to find a parent_task_id when nesting. For 'remind me to X at <time>' requests, set scheduled_at to the requested reminder time AND remind=true — Sajni emails the user at that time.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"title":            str("Required. Short title."),
				"description":      str("Optional details."),
				"priority":         str("'high' | 'medium' | 'low'. Default 'medium'."),
				"due_date":         str("Optional ISO date YYYY-MM-DD."),
				"week_of":          str("Optional. Makes this a week-scoped task with no specific day — pass the ISO Monday date (YYYY-MM-DD) of the target week. Shows in the 'This Week' list. Mutually exclusive with due_date."),
				"month_of":         str("Optional. Makes this a month goal with no specific day — pass the ISO 1st-of-month date (YYYY-MM-DD), e.g. 2026-06-01. Shows in the 'This Month' list; the user breaks it into dated child sessions (create them with parent_task_id + due_date). Mutually exclusive with due_date and week_of. Don't invent session dates — create only the goal unless the user gives a schedule."),
				"scheduled_at":     str("Optional ISO timestamp with offset, e.g. 2026-05-30T17:00:00+05:30. The event/reminder time."),
				"remind":           boolean("Optional. If true, email the user at scheduled_at. Requires scheduled_at."),
				"notify_emails":    arrayOf(str(""), "Optional. Extra email addresses to also notify when this task's reminders fire (e.g. a friend for a meet-up). Max 3."),
				"duration_minutes": intg("Optional. Default 30."),
				"list_id":          intg("Optional. Place the task inside this user list."),
				"parent_task_id":   intg("Optional. Make this a subtask of the given task id."),
				"important":        boolean("Optional. Star the task (Important smart list)."),
				"steps":            arrayOf(str(""), "Optional inline checklist (array of step text strings)."),
				"tags":             arrayOf(str(""), "Optional tag list."),
			}, "title"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createTaskTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "create_task_list",
			Description: "Create a new top-level task list (grouping). Useful when the user says 'put these under a new Work list' or similar.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"name":  str("Required. Display name."),
				"color": str("Optional hex like '#2D5A4F'."),
				"icon":  str("Optional icon hint (free-form, frontend may map)."),
			}, "name"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createTaskListTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "update_task",
			Description: "Edit an existing task in place: title, description, priority, due_date, and/or move it between lists. This is the only way to move or promote a task to a different list — pass list_id (resolve names via list_task_lists), or to_inbox=true to clear its list. Only the fields you pass change. For reminder/scheduled-time edits use reschedule_task instead.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":             intg("Required. Task id (use list_tasks to find it)."),
				"title":          str("Optional. New title."),
				"description":    str("Optional. New details."),
				"priority":       str("Optional. 'high' | 'medium' | 'low'."),
				"due_date":       str("Optional. New due date, ISO YYYY-MM-DD."),
				"list_id":        intg("Optional. Move the task into this list."),
				"parent_task_id": intg("Optional. Re-parent as a subtask of this task id."),
				"to_inbox":       boolean("Optional. true clears the task's list (promote to Inbox). Ignored when list_id is set."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return updateTaskTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "set_task_important",
			Description: "Toggle or set the 'important' star on a task.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":        intg("Task id."),
				"important": boolean("true to star, false to unstar."),
			}, "id", "important"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				if id == 0 {
					return nil, nil, fmt.Errorf("missing id")
				}
				val := argBool(args, "important", false)
				if _, err := d.ExecContext(ctx, `UPDATE tasks SET important=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3`, val, id, uid); err != nil {
					return nil, nil, err
				}
				return map[string]any{"id": id, "important": val},
					map[string]any{"kind": "task_updated", "id": id, "route": "/tasks"}, nil
			},
		},
		{
			Name:        "complete_task",
			Description: "Mark a task as done.",
			Mutating:    true,
			Schema:      obj(map[string]*genai.Schema{"id": intg("Task id.")}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				if id == 0 {
					return nil, nil, fmt.Errorf("missing id")
				}
				_, err := d.Exec(`UPDATE tasks SET status='done', updated_at=NOW() WHERE id=$1 AND user_id=$2`, id, uid)
				if err != nil {
					return nil, nil, err
				}
				// Completing the last open session of a month goal finishes it.
				d.Exec(`
					UPDATE tasks g SET status='done', updated_at=NOW()
					 WHERE g.user_id=$2 AND g.month_of IS NOT NULL
					   AND g.status NOT IN ('done','scratched')
					   AND g.id = (SELECT parent_task_id FROM tasks WHERE id=$1 AND user_id=$2)
					   AND EXISTS (SELECT 1 FROM tasks c WHERE c.parent_task_id=g.id AND c.user_id=$2)
					   AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_task_id=g.id AND c.user_id=$2 AND c.status NOT IN ('done','scratched'))`,
					id, uid)
				return map[string]any{"id": id, "status": "done"},
					map[string]any{"kind": "task_completed", "id": id}, nil
			},
		},
		{
			Name:        "reschedule_task",
			Description: "Move an existing task's due date and/or time. The main way to clear 'missed' (overdue) tasks — set due_date to a future day. Resolve relative dates against get_current_context. Moving a past due_date forward is recorded as a reschedule in the task's lifecycle (not a miss). Pass scheduled_at to also set a time + reminder; due_date follows the scheduled day if omitted.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":           intg("Required. Task id (use list_tasks, smart='missed' to find overdue ones)."),
				"due_date":     str("New due date, ISO YYYY-MM-DD."),
				"scheduled_at": str("Optional new event time, ISO with offset e.g. 2026-06-06T17:00:00+05:30. Re-arms the reminder."),
				"remind":       boolean("Optional. Email the user at scheduled_at."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return rescheduleTaskTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "scratch_task",
			Description: "Scratch (abandon) a task the user has decided not to do — it drops out of open lists, My Day, Missed, and reminders, but is kept (struck-through) and reversible. Prefer this over delete_task when the user says 'drop', 'skip', 'never mind', 'cancel' a task. Pass unscratch=true to restore it to 'todo'.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":        intg("Required. Task id."),
				"unscratch": boolean("Optional. true restores a scratched task back to 'todo'."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return scratchTaskTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "delete_task",
			Description: "Delete a task permanently.",
			Mutating:    true,
			Schema:      obj(map[string]*genai.Schema{"id": intg("Task id.")}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				if id == 0 {
					return nil, nil, fmt.Errorf("missing id")
				}
				_, err := d.Exec(`DELETE FROM tasks WHERE id=$1 AND user_id=$2`, id, uid)
				if err != nil {
					return nil, nil, err
				}
				return map[string]any{"id": id, "deleted": true},
					map[string]any{"kind": "task_deleted", "id": id}, nil
			},
		},
		{
			Name:        "add_task_reminder",
			Description: "Add a reminder to a task at a specific time. A task can carry multiple reminders on any date — each emails the user at that instant, independent of the task's own time. Resolve relative times against get_current_context; use list_tasks to find the task id.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"task_id":   intg("Required. Task to remind about."),
				"remind_at": str("Required. ISO timestamp with offset, e.g. 2026-06-27T09:00:00+05:30."),
			}, "task_id", "remind_at"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				tid := argInt(args, "task_id", 0)
				at := strings.TrimSpace(argStr(args, "remind_at"))
				if tid == 0 || at == "" {
					return nil, nil, fmt.Errorf("missing task_id or remind_at")
				}
				var owned int
				d.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id=$1 AND user_id=$2`, tid, uid).Scan(&owned)
				if owned != 1 {
					return nil, nil, fmt.Errorf("task not found")
				}
				var rid int64
				var remindAt time.Time
				if err := d.QueryRowContext(ctx, `INSERT INTO task_reminders (user_id, task_id, remind_at) VALUES ($1,$2,$3) RETURNING id, remind_at`, uid, tid, at).Scan(&rid, &remindAt); err != nil {
					return nil, nil, err
				}
				enqueueMultiReminder(ctx, rid, remindAt)
				return map[string]any{"id": rid, "task_id": tid, "remind_at": at},
					map[string]any{"kind": "task_updated", "id": tid, "route": "/tasks"}, nil
			},
		},
		{
			Name:        "create_habit",
			Description: "Create a new habit to track.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"name":      str("Required."),
				"frequency": str("'daily' | 'weekly'. Default 'daily'."),
				"color":     str("Hex like '#2D5A4F'."),
			}, "name"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createHabitTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "log_habit",
			Description: "Log that a habit was completed on a given day.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"habit_id": intg("Habit id."),
				"date":     str("ISO date. Defaults to today."),
			}, "habit_id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				hid := argInt(args, "habit_id", 0)
				if hid == 0 {
					return nil, nil, fmt.Errorf("missing habit_id")
				}
				date := argStr(args, "date")
				if date == "" {
					date = time.Now().Format("2006-01-02")
				}
				_, err := d.Exec(`INSERT INTO habit_logs (user_id, habit_id, logged_date) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, uid, hid, date)
				if err != nil {
					return nil, nil, err
				}
				return map[string]any{"habit_id": hid, "date": date},
					map[string]any{"kind": "habit_logged", "habit_id": hid, "date": date}, nil
			},
		},
		{
			Name:        "create_memo",
			Description: "Capture a quick memo / note-to-self.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"content": str("The memo body."),
				"pinned":  boolean("Pin to top."),
				"tags":    arrayOf(str(""), "Optional tags."),
			}, "content"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createMemoTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "create_journal_entry",
			Description: "Create or replace a journal entry for the given date. Content is stored as a markdown blob; optional location_label (short, e.g. 'Cinepolis, Vashi') powers the location pill.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"date":           str("ISO date. Defaults to today."),
				"mood":           str("e.g. 'happy', 'focused', 'tired'."),
				"content":        str("The entry body in markdown."),
				"location_label": str("Optional short place label like 'Cinepolis, Vashi'."),
			}, "content"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createJournalTool(ctx, d, store, uid, args)
			},
		},
		{
			Name:        "create_note",
			Description: "Create a markdown note in the user's notes. Optional folder path (e.g. 'work/q2'). If title is empty, it's derived from the first content line.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"title":       str("Optional. If empty, derived from first content line."),
				"content":     str("Markdown body."),
				"folder":      str("Optional. Folder path like 'work/q2'."),
				"description": str("Optional one-line summary shown on the notes home cards."),
				"tags":        arrayOf(str(""), "Optional tag list."),
			}, "content"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createNoteTool(ctx, d, store, uid, args)
			},
		},
		{
			Name:        "create_folder",
			Description: "Create a folder under the user's notes tree. Path uses '/' separators (e.g. 'work/q2').",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"path": str("Required. Folder path like 'work/q2'."),
			}, "path"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createFolderTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "add_media",
			Description: "Add a movie / show / book to the user's library. Call this whenever the user mentions a title they have consumed, are consuming, or plan to consume. Status mapping: 'done' for past-tense (\"I watched\", \"already saw\", \"finished\", \"just read\"), 'watching' for in-progress (\"halfway through\", \"on episode 4\"), 'pending' for intent (\"want to watch\", \"need to read\"). If you have an external_id from tmdb_search, pass it along with poster_url, year, release_date, and genre to fully populate the entry.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"title":        str("Required. Exact title."),
				"type":         str("'movie' | 'show' | 'book'."),
				"status":       str("'pending' | 'watching' | 'done'. Pick based on the user's wording — past tense ⇒ 'done'. Default 'pending' only when unclear."),
				"external_id":  str("Optional TMDB external id from tmdb_search."),
				"year":         intg("Optional release year."),
				"release_date": str("Optional release date from tmdb_search, YYYY-MM-DD."),
				"genre":        str("Optional."),
				"poster_url":   str("Optional."),
				"rating":       intg("Optional 1–5 star rating, only when user expressed one."),
			}, "title", "type"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return addMediaTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "create_bookmark",
			Description: "Save a link (website, video, article) to the user's bookmarks. New bookmarks land in the read-later queue (unread).",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"url":   str("Required. Full http(s) URL."),
				"title": str("Optional. If omitted the host is used."),
				"note":  str("Optional context. #tags are extracted."),
			}, "url"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createBookmarkTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "update_bookmark",
			Description: "Update a bookmark: mark read/unread, archive/unarchive, or change title/note. Use list_bookmarks first to find the id.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":       intg("Required. Bookmark id."),
				"unread":   boolean("false = mark as read."),
				"archived": boolean("true = archive."),
				"title":    str("Optional new title."),
				"note":     str("Optional new note."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return updateBookmarkTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_billers",
			Description: "List the user's billers / subscriptions with next due date, amount, account, and auto-renew flag. Use this to answer 'what bills are coming up?' or before creating a transaction the user might be paying via a biller.",
			Schema: obj(map[string]*genai.Schema{
				"include_archived": boolean("Include archived rows."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listBillersTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "create_biller",
			Description: "Create a recurring bill or subscription. Resolve frequency to one of weekly | fortnightly | monthly | bimonthly. If auto_renew=true an account_id is required; the cron will post the expense automatically each cycle.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"name":            str("Required. e.g. 'Netflix' or 'Electricity'."),
				"amount":          num("Required positive amount."),
				"frequency":       str("weekly | fortnightly | monthly | bimonthly. Default monthly."),
				"next_due_date":   str("ISO date for the next charge. Defaults to today."),
				"account_id":      intg("Account that pays this. Required when auto_renew=true."),
				"category_id":     intg("Optional expense category."),
				"is_subscription": boolean("True for streaming-type recurring services."),
				"auto_renew":      boolean("If true, cron posts the expense automatically on/after due date."),
				"remind_task":     boolean("If true (and not auto_renew), the biller cron spawns a 'Pay {name}' reminder task each cycle that emails the user."),
				"alert_days":      intg("Days before due_date to alert (default 3)."),
				"notes":           str("Optional."),
			}, "name", "amount"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createBillerTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "pay_biller",
			Description: "Mark a biller as paid for its current cycle: posts an expense transaction against the biller's account and rolls next_due_date forward by one period. Idempotent per (biller, due_date).",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"biller_id": intg("Required."),
				"amount":    num("Override the biller's amount for this cycle."),
				"paid_date": str("ISO date. Defaults to today."),
			}, "biller_id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return payBillerTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "time_travel",
			Description: "Semantic event lookup across journals, memos, notes, transactions, media, and journal location pills. Use for 'when did I last X?', 'how long since I met Y?', 'what was that place I went to in March?'. Returns ranked matches with date + a short context excerpt.",
			Schema: obj(map[string]*genai.Schema{
				"query":     str("Required. Natural language query e.g. 'last time I met Jay'."),
				"types":     arrayOf(str(""), "Optional whitelist: journal, memo, note, transaction, media, location."),
				"date_from": str("Optional ISO lower bound."),
				"date_to":   str("Optional ISO upper bound."),
				"limit":     intg("Default 10."),
			}, "query"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return timeTravelTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "generate_theme",
			Description: "Generate a new Material 3 color theme from a natural-language prompt (e.g. 'dusty rose calm dark-leaning' or 'forest morning'). Picks primary, secondary, tertiary, and neutral seed colors. Saves the theme to the user's profile; pass activate=true to make it the active one immediately.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"prompt":    str("Required. Free-form description of the mood, vibe, or palette."),
				"activate":  boolean("If true, switch to this theme right away. Default false."),
				"mode_pref": str("'auto' | 'light' | 'dark'. Default 'auto'."),
			}, "prompt"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return generateThemeTool(ctx, s, uid, args)
			},
		},
		{
			Name:        "list_themes",
			Description: "List the user's saved color themes (built-ins, AI-generated, and manual). Useful before activating a theme by name.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listThemesTool(ctx, d, uid)
			},
		},
		{
			Name:        "activate_theme",
			Description: "Switch the user to a saved theme by id. Use list_themes first to find the id.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id": intg("Theme id."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return activateThemeTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_insights",
			Description: "List the user's generated cross-module insights (mood vs task completion, spending spikes, habit streak correlations, etc). Optionally filter by window.",
			Schema: obj(map[string]*genai.Schema{
				"window": str("Optional: 1w | 2w | 1m | 6m | 1y."),
				"limit":  intg("Default 10."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listInsightsTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_thinking_projects",
			Description: "List the user's Thinking projects (containers of typed thought-cards). Use to find an existing project before adding a thought, or to summarize what the user is currently thinking about.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listThinkingProjectsTool(ctx, d, uid)
			},
		},
		{
			Name:        "get_thinking_project",
			Description: "Read one Thinking project with all its cards. Use when the user references a project by name and you need its current cards before answering or adding a thought.",
			Schema: obj(map[string]*genai.Schema{
				"id": intg("Project id."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return getThinkingProjectAITool(ctx, d, uid, args)
			},
		},
		{
			Name:        "create_thinking_project",
			Description: "Start a new Thinking project. Use when the user asks to 'start thinking about X' or 'open a project for Y'. Returns the new project id so you can immediately add_thought cards to it.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"title":       str("Required. Short title for the project."),
				"description": str("Optional one-line description."),
			}, "title"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createThinkingProjectAITool(ctx, d, uid, args)
			},
		},
		{
			Name:        "add_thought",
			Description: "Add a typed thought-card to a Thinking project. Pick the most fitting kind: 'note' (fallback), 'entity', 'question', 'idea', 'reflection', 'claim', 'fact', 'hypothesis', 'evidence', 'contradiction', 'decision', 'todo'. The backend asynchronously enriches the card with meaning + connections to other cards.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"project_id": intg("Required. The Thinking project id (use list_thinking_projects)."),
				"kind":       str("One of: note | entity | question | idea | reflection | claim | fact | hypothesis | evidence | contradiction | decision | todo. Default 'note'."),
				"content":    str("Required. The thought body (markdown ok)."),
			}, "project_id", "content"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return addThoughtTool(ctx, s, uid, args)
			},
		},
		{
			Name:        "create_transaction",
			Description: "Record a finance transaction against an existing account and assign a category. Use list_finance_accounts to get account_id. Specify either category_id or category_name; if category_name is specified, the backend will auto-match it to the closest existing category.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"account_id":    intg("Required. The target account ID."),
				"category_id":   intg("Optional category identifier."),
				"category_name": str("Optional category name (e.g. 'Food', 'Groceries', 'Rent') to auto-match on backend."),
				"type":          str("'expense' | 'income'. Default 'expense'."),
				"amount":        num("Positive amount."),
				"description":   str("What it was for."),
				"date":          str("ISO date. Defaults to today."),
			}, "account_id", "amount"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createTxnTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "update_transaction",
			Description: "Update an existing finance transaction by id — change its account, category, amount, type, description, or date. To move it to another account pass account_id (balances recompute automatically). To recategorize, pass category_name (auto-matched against existing categories, same as create) or category_id. Use list_finance_transactions to find the id first.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":            intg("Required. Transaction id to update."),
				"account_id":    intg("Optional. Move the transaction to this account id."),
				"category_id":   intg("Optional category id."),
				"category_name": str("Optional category name to auto-match (e.g. 'Groceries', 'Rent')."),
				"type":          str("Optional 'expense' | 'income'."),
				"amount":        num("Optional positive amount."),
				"description":   str("Optional new description."),
				"date":          str("Optional ISO date YYYY-MM-DD."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return updateTxnTool(ctx, d, uid, args)
			},
		},
	}
}

// ----- handler implementations -----

func getCurrentContextTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	now := userTZNow(ctx, d, uid)
	today := now.Format("2006-01-02")
	out := map[string]any{
		"today":     today,
		"weekday":   now.Weekday().String(),
		"timestamp": now.Format(time.RFC3339),
	}
	// "open" excludes scratched (abandoned) as well as done.
	var openTasks, dueToday, missed, habitsTotal, habitsDone, memos7d int
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status NOT IN ('done','scratched')`, uid).Scan(&openTasks)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status NOT IN ('done','scratched') AND due_date=$2`, uid, today).Scan(&dueToday)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status NOT IN ('done','scratched') AND due_date < $2`, uid, today).Scan(&missed)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM habits WHERE user_id=$1`, uid).Scan(&habitsTotal)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM habit_logs WHERE user_id=$1 AND logged_date=$2`, uid, today).Scan(&habitsDone)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM memos WHERE user_id=$1 AND created_at > NOW() - INTERVAL '7 days'`, uid).Scan(&memos7d)
	out["open_tasks"] = openTasks
	out["tasks_due_today"] = dueToday
	out["tasks_missed"] = missed
	out["habits"] = map[string]any{"total": habitsTotal, "done_today": habitsDone}
	out["recent_memos_7d"] = memos7d
	return out, nil, nil
}

func listTasksTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	clauses := []string{"user_id=$1"}
	vals := []any{uid}

	// "active" excludes scratched (abandoned) and done. Dates are the user's
	// local day, not the server's UTC CURRENT_DATE.
	const active = "status NOT IN ('done','scratched')"
	today := userTZNow(ctx, d, uid).Format("2006-01-02")
	switch argStr(args, "smart") {
	case "my_day":
		clauses = append(clauses, fmt.Sprintf("due_date = $%d", len(vals)+1), active)
		vals = append(vals, today)
	case "important":
		clauses = append(clauses, "important = TRUE", active)
	case "planned":
		clauses = append(clauses, "due_date IS NOT NULL", active)
	case "week":
		// Week-scoped tasks for the current Monday-anchored week.
		now := userTZNow(ctx, d, uid)
		mon := now.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7)).Format("2006-01-02")
		clauses = append(clauses, fmt.Sprintf("week_of = $%d", len(vals)+1), active)
		vals = append(vals, mon)
	case "month":
		// Month goals for the current 1st-anchored month.
		now := userTZNow(ctx, d, uid)
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
		clauses = append(clauses, fmt.Sprintf("month_of = $%d", len(vals)+1), active)
		vals = append(vals, first)
	case "missed":
		// Child sessions of a month goal roll up to the goal — never list them
		// here individually (mirrors the REST Missed list).
		clauses = append(clauses, fmt.Sprintf("due_date < $%d", len(vals)+1), active,
			"NOT EXISTS (SELECT 1 FROM tasks mgp WHERE mgp.id = tasks.parent_task_id AND mgp.month_of IS NOT NULL)")
		vals = append(vals, today)
	case "inbox":
		clauses = append(clauses, "list_id IS NULL")
	case "all":
		// no extra filter
	}

	if lid := argInt(args, "list_id", 0); lid != 0 {
		clauses = append(clauses, fmt.Sprintf("list_id=$%d", len(vals)+1))
		vals = append(vals, lid)
	}

	// parent_id: 0 = top-level only (default unless smart/list filter set);
	// non-zero = children of that parent.
	if pid := argInt(args, "parent_id", 0); pid != 0 {
		clauses = append(clauses, fmt.Sprintf("parent_task_id=$%d", len(vals)+1))
		vals = append(vals, pid)
	} else if argStr(args, "smart") == "" && argInt(args, "list_id", 0) == 0 {
		clauses = append(clauses, "parent_task_id IS NULL")
	}

	if s := argStr(args, "status"); s != "" {
		clauses = append(clauses, fmt.Sprintf("status=$%d", len(vals)+1))
		vals = append(vals, s)
	}
	if p := argStr(args, "priority"); p != "" {
		clauses = append(clauses, fmt.Sprintf("priority=$%d", len(vals)+1))
		vals = append(vals, p)
	}
	if argBool(args, "important", false) {
		clauses = append(clauses, "important=TRUE")
	}
	if df := argStr(args, "due_from"); df != "" {
		clauses = append(clauses, fmt.Sprintf("due_date >= $%d", len(vals)+1))
		vals = append(vals, df)
	}
	if dt := argStr(args, "due_to"); dt != "" {
		clauses = append(clauses, fmt.Sprintf("due_date <= $%d", len(vals)+1))
		vals = append(vals, dt)
	}
	limit := argInt(args, "limit", 50)
	q := `SELECT id,title,COALESCE(description,''),status,priority,
	             COALESCE(due_date::text,''),COALESCE(scheduled_at::text,''),
	             COALESCE(duration_minutes,30),
	             list_id,parent_task_id,important,
	             (SELECT COUNT(*) FROM tasks c WHERE c.parent_task_id = tasks.id)
	      FROM tasks WHERE ` + strings.Join(clauses, " AND ") +
		fmt.Sprintf(` ORDER BY (status='done') ASC, important DESC NULLS LAST, due_date NULLS LAST, created_at DESC LIMIT %d`, limit)
	rows, err := d.QueryContext(ctx, q, vals...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var title, desc, status, priority, due, sched string
		var dur int
		var listID, parentID sql.NullInt64
		var important bool
		var subCount int
		rows.Scan(&id, &title, &desc, &status, &priority, &due, &sched, &dur,
			&listID, &parentID, &important, &subCount)
		row := map[string]any{
			"id": id, "title": title, "description": desc,
			"status": status, "priority": priority,
			"due_date": due, "scheduled_at": sched, "duration_minutes": dur,
			"important": important, "subtask_count": subCount,
		}
		if listID.Valid {
			row["list_id"] = listID.Int64
		}
		if parentID.Valid {
			row["parent_task_id"] = parentID.Int64
		}
		out = append(out, row)
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func listTaskListsTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT l.id, l.name, l.color, l.icon,
		       COALESCE(c.cnt, 0)
		FROM task_lists l
		LEFT JOIN (
			SELECT list_id, COUNT(*) AS cnt FROM tasks
			WHERE user_id=$1 AND parent_task_id IS NULL AND status NOT IN ('done','scratched')
			GROUP BY list_id
		) c ON c.list_id = l.id
		WHERE l.user_id=$1
		ORDER BY l.sort_order ASC, l.id ASC`, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, color, icon string
		var cnt int
		rows.Scan(&id, &name, &color, &icon, &cnt)
		out = append(out, map[string]any{
			"id": id, "name": name, "color": color, "icon": icon, "open_count": cnt,
		})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func createTaskListTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	name := argStr(args, "name")
	if name == "" {
		return nil, nil, fmt.Errorf("missing name")
	}
	color := argStr(args, "color")
	if color == "" {
		color = "#2D5A4F"
	}
	icon := argStr(args, "icon")
	if icon == "" {
		icon = "list"
	}
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO task_lists (user_id, name, color, icon, sort_order)
		VALUES ($1, $2, $3, $4, COALESCE((SELECT MAX(sort_order)+1 FROM task_lists WHERE user_id=$1), 0))
		RETURNING id`, uid, name, color, icon).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "name": name},
		map[string]any{"kind": "task_list_created", "id": id, "title": name, "route": "/tasks"}, nil
}

func listHabitsTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	today := time.Now().Format("2006-01-02")
	rows, err := d.QueryContext(ctx, `
		SELECT h.id, h.name, h.frequency,
		  (SELECT COUNT(*) FROM habit_logs l WHERE l.habit_id=h.id AND l.user_id=$1) AS total_logs,
		  EXISTS(SELECT 1 FROM habit_logs l WHERE l.habit_id=h.id AND l.user_id=$1 AND l.logged_date=$2) AS done_today
		FROM habits h WHERE h.user_id=$1 ORDER BY h.id`, uid, today)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, total int64
		var name, freq string
		var done bool
		rows.Scan(&id, &name, &freq, &total, &done)
		out = append(out, map[string]any{
			"id": id, "name": name, "frequency": freq,
			"total_logs": total, "done_today": done,
		})
	}
	return map[string]any{"items": out, "count": len(out), "today": today}, nil, nil
}

func listJournalTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	clauses := []string{"user_id=$1"}
	vals := []any{uid}
	if df := argStr(args, "date_from"); df != "" {
		clauses = append(clauses, fmt.Sprintf("date >= $%d", len(vals)+1))
		vals = append(vals, df)
	}
	if dt := argStr(args, "date_to"); dt != "" {
		clauses = append(clauses, fmt.Sprintf("date <= $%d", len(vals)+1))
		vals = append(vals, dt)
	}
	limit := argInt(args, "limit", 20)
	q := `SELECT id, date::text, COALESCE(mood,'') FROM journal_entries WHERE ` +
		strings.Join(clauses, " AND ") +
		fmt.Sprintf(` ORDER BY date DESC LIMIT %d`, limit)
	rows, err := d.QueryContext(ctx, q, vals...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var date, mood string
		rows.Scan(&id, &date, &mood)
		out = append(out, map[string]any{"id": id, "date": date, "mood": mood})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func getJournalTool(ctx context.Context, d *db.DB, store storage.Storage, uid string, date string) (any, map[string]any, error) {
	if date == "" {
		return nil, nil, fmt.Errorf("missing date")
	}
	var id int64
	var blobKey, mood string
	err := d.QueryRowContext(ctx, `SELECT id, blob_key, COALESCE(mood,'') FROM journal_entries WHERE user_id=$1 AND date=$2`, uid, date).
		Scan(&id, &blobKey, &mood)
	if err == sql.ErrNoRows {
		return nil, nil, fmt.Errorf("no entry for %s", date)
	}
	if err != nil {
		return nil, nil, err
	}
	content := ""
	if blobKey != "" {
		data, _, e := store.Get(ctx, blobKey)
		if e == nil {
			content = string(data)
		}
	}
	return map[string]any{"id": id, "date": date, "mood": mood, "content": content}, nil, nil
}

func listMemosTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	limit := argInt(args, "limit", 20)
	q := argStr(args, "query")
	rows, err := d.QueryContext(ctx, `
		SELECT id, content, pinned, created_at
		FROM memos
		WHERE user_id=$1 AND ($2 = '' OR content ILIKE '%' || $2 || '%')
		ORDER BY pinned DESC, created_at DESC LIMIT $3`, uid, q, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var content, created string
		var pinned bool
		rows.Scan(&id, &content, &pinned, &created)
		out = append(out, map[string]any{"id": id, "content": content, "pinned": pinned, "created_at": created})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

// mediaTasteTool digests the library into a small taste profile so the
// model can recommend in the user's voice instead of pulling from
// generic popularity. Cheap enough to call on every recommendation
// turn (a handful of small queries).
func mediaTasteTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	out := map[string]any{}

	// Top-rated finished titles (weighted "what they loved").
	rows, err := d.QueryContext(ctx, `
		SELECT title, type, COALESCE(genre,''), COALESCE(rating,0), COALESCE(year,0)
		  FROM media
		 WHERE user_id=$1 AND rating IS NOT NULL AND rating >= 4
		 ORDER BY rating DESC, updated_at DESC LIMIT 12`, uid)
	favs := []map[string]any{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var title, mtype, genre string
			var rating, year int
			rows.Scan(&title, &mtype, &genre, &rating, &year)
			favs = append(favs, map[string]any{"title": title, "type": mtype, "genre": genre, "rating": rating, "year": year})
		}
	}
	out["favourites"] = favs

	// Genre affinity: sum of ratings per genre token (cheap weighting).
	genreScore := map[string]int{}
	genreCount := map[string]int{}
	grows, err := d.QueryContext(ctx, `
		SELECT COALESCE(genre,''), COALESCE(rating,0), status
		  FROM media WHERE user_id=$1`, uid)
	if err == nil {
		defer grows.Close()
		for grows.Next() {
			var genre, status string
			var rating int
			grows.Scan(&genre, &rating, &status)
			if genre == "" {
				continue
			}
			for _, g := range strings.Split(genre, ",") {
				g = strings.TrimSpace(g)
				if g == "" {
					continue
				}
				w := rating
				if w == 0 {
					if status == "complete" {
						w = 3 // completion implies some signal
					} else {
						continue
					}
				}
				genreScore[g] += w
				genreCount[g]++
			}
		}
	}
	type gentry struct {
		Name  string `json:"genre"`
		Score int    `json:"weight"`
		Count int    `json:"count"`
	}
	gtop := []gentry{}
	for g, s := range genreScore {
		gtop = append(gtop, gentry{g, s, genreCount[g]})
	}
	// Insertion sort by score desc (small list).
	for i := 1; i < len(gtop); i++ {
		for j := i; j > 0 && gtop[j].Score > gtop[j-1].Score; j-- {
			gtop[j], gtop[j-1] = gtop[j-1], gtop[j]
		}
	}
	if len(gtop) > 8 {
		gtop = gtop[:8]
	}
	out["top_genres"] = gtop

	// Completion vs. drop ratio per type — a coarse "how do they
	// engage" signal.
	stats := map[string]any{}
	srows, err := d.QueryContext(ctx, `
		SELECT type,
		       COUNT(*) FILTER (WHERE status='complete') AS completed,
		       COUNT(*) FILTER (WHERE status='dropped' OR status='scratched') AS dropped,
		       COUNT(*) FILTER (WHERE status='in_progress') AS in_progress,
		       COUNT(*) AS total
		  FROM media WHERE user_id=$1 GROUP BY type`, uid)
	if err == nil {
		defer srows.Close()
		for srows.Next() {
			var mtype string
			var done, dropped, inProg, total int
			srows.Scan(&mtype, &done, &dropped, &inProg, &total)
			stats[mtype] = map[string]any{
				"completed": done, "dropped": dropped, "in_progress": inProg, "total": total,
			}
		}
	}
	out["engagement"] = stats

	// Last 5 completed — fresh "context for what they just enjoyed".
	rrows, err := d.QueryContext(ctx, `
		SELECT m.title, m.type, COALESCE(m.genre,''), COALESCE(m.rating,0), e.created_at::text
		  FROM media m
		  JOIN media_events e ON e.media_id=m.id AND e.user_id=m.user_id AND e.kind='completed'
		 WHERE m.user_id=$1
		 ORDER BY e.created_at DESC LIMIT 5`, uid)
	recent := []map[string]any{}
	if err == nil {
		defer rrows.Close()
		for rrows.Next() {
			var title, mtype, genre, when string
			var rating int
			rrows.Scan(&title, &mtype, &genre, &rating, &when)
			recent = append(recent, map[string]any{
				"title": title, "type": mtype, "genre": genre, "rating": rating, "completed_at": when,
			})
		}
	}
	out["recently_completed"] = recent

	// Library size — also tells the model how much signal we have.
	var libSize int
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM media WHERE user_id=$1`, uid).Scan(&libSize)
	out["library_size"] = libSize

	return out, nil, nil
}

func listMediaTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	clauses := []string{"user_id=$1"}
	vals := []any{uid}
	if s := argStr(args, "status"); s != "" {
		clauses = append(clauses, fmt.Sprintf("status=$%d", len(vals)+1))
		vals = append(vals, s)
	}
	if t := argStr(args, "type"); t != "" {
		clauses = append(clauses, fmt.Sprintf("type=$%d", len(vals)+1))
		vals = append(vals, t)
	}
	limit := argInt(args, "limit", 30)
	q := `SELECT id, title, type, status, COALESCE(rating,0), COALESCE(genre,''), COALESCE(year,0)
	      FROM media WHERE ` + strings.Join(clauses, " AND ") +
		fmt.Sprintf(` ORDER BY updated_at DESC LIMIT %d`, limit)
	rows, err := d.QueryContext(ctx, q, vals...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var title, mtype, status, genre string
		var rating, year int
		rows.Scan(&id, &title, &mtype, &status, &rating, &genre, &year)
		out = append(out, map[string]any{"id": id, "title": title, "type": mtype, "status": status, "rating": rating, "genre": genre, "year": year})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func listAccountsTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT a.id, a.name, a.type, a.institution, a.currency,
		       a.opening_balance + COALESCE((SELECT SUM(CASE WHEN t.type='income' THEN t.amount ELSE -t.amount END)
		                                     FROM fin_transactions t WHERE t.account_id=a.id AND t.user_id=a.user_id),0) AS balance
		FROM fin_accounts a WHERE a.user_id=$1 AND a.archived=FALSE ORDER BY a.id`, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, atype, inst, currency string
		var balance float64
		rows.Scan(&id, &name, &atype, &inst, &currency, &balance)
		out = append(out, map[string]any{"id": id, "name": name, "type": atype, "institution": inst, "currency": currency, "balance": balance})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func listTxnsTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	clauses := []string{"t.user_id=$1"}
	vals := []any{uid}
	if a := argInt(args, "account_id", 0); a > 0 {
		clauses = append(clauses, fmt.Sprintf("t.account_id=$%d", len(vals)+1))
		vals = append(vals, a)
	}
	if df := argStr(args, "date_from"); df != "" {
		clauses = append(clauses, fmt.Sprintf("(t.txn_at AT TIME ZONE 'Asia/Kolkata')::date >= $%d", len(vals)+1))
		vals = append(vals, df)
	}
	if dt := argStr(args, "date_to"); dt != "" {
		clauses = append(clauses, fmt.Sprintf("(t.txn_at AT TIME ZONE 'Asia/Kolkata')::date <= $%d", len(vals)+1))
		vals = append(vals, dt)
	}
	limit := argInt(args, "limit", 50)
	q := `SELECT t.id, t.account_id, t.type, t.amount, t.description, (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date::text, t.category_id, COALESCE(c.name, '')
	      FROM fin_transactions t
	      LEFT JOIN fin_categories c ON c.id = t.category_id
	      WHERE ` + strings.Join(clauses, " AND ") +
		fmt.Sprintf(` ORDER BY t.txn_at DESC LIMIT %d`, limit)
	rows, err := d.QueryContext(ctx, q, vals...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, acct int64
		var ttype, desc, date string
		var amount float64
		var catID sql.NullInt64
		var catName string
		rows.Scan(&id, &acct, &ttype, &amount, &desc, &date, &catID, &catName)
		row := map[string]any{
			"id": id, "account_id": acct, "type": ttype, "amount": amount,
			"description": desc, "date": date, "category_name": catName,
		}
		if catID.Valid {
			row["category_id"] = catID.Int64
		}
		out = append(out, row)
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func runSearchTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	q := argStr(args, "q")
	types := argStrSlice(args, "types")
	want := func(t string) bool {
		if len(types) == 0 {
			return true
		}
		for _, x := range types {
			if x == t {
				return true
			}
		}
		return false
	}
	like := "%" + q + "%"
	out := []map[string]any{}
	add := func(t string, sqlText string) {
		rows, err := d.QueryContext(ctx, sqlText, uid, q, like)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var title, sub string
			rows.Scan(&id, &title, &sub)
			out = append(out, map[string]any{"type": t, "id": id, "title": title, "subtitle": sub})
		}
	}
	if want("memo") {
		add("memo", `SELECT id, LEFT(content,80), '' FROM memos WHERE user_id=$1 AND ($2='' OR content ILIKE $3) ORDER BY pinned DESC, updated_at DESC LIMIT 10`)
	}
	if want("task") {
		add("task", `SELECT id, title, status FROM tasks WHERE user_id=$1 AND ($2='' OR title ILIKE $3 OR description ILIKE $3) ORDER BY updated_at DESC LIMIT 10`)
	}
	if want("note") {
		add("note", `SELECT id, title, COALESCE(folder,'') FROM notes WHERE user_id=$1 AND ($2='' OR title ILIKE $3 OR folder ILIKE $3) ORDER BY updated_at DESC LIMIT 10`)
	}
	if want("habit") {
		add("habit", `SELECT id, name, frequency FROM habits WHERE user_id=$1 AND ($2='' OR name ILIKE $3) LIMIT 10`)
	}
	if want("media") {
		add("media", `SELECT id, title, type FROM media WHERE user_id=$1 AND ($2='' OR title ILIKE $3 OR genre ILIKE $3) ORDER BY updated_at DESC LIMIT 10`)
	}
	if want("transaction") {
		add("transaction", `
			SELECT t.id,
			       t.description || ' - ' || (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date::text AS title,
			       a.currency || ' ' || t.amount::text AS subtitle
			FROM fin_transactions t
			JOIN fin_accounts a ON a.id = t.account_id
			WHERE t.user_id=$1 AND ($2='' OR t.description ILIKE $3)
			ORDER BY t.txn_at DESC LIMIT 10`)
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func tmdbReleaseState(releaseDate string) string {
	releaseDate = strings.TrimSpace(releaseDate)
	if len(releaseDate) < len("2006-01-02") {
		return "unknown"
	}
	releaseDate = releaseDate[:len("2006-01-02")]
	if _, err := time.Parse("2006-01-02", releaseDate); err != nil {
		return "unknown"
	}
	if releaseDate <= time.Now().Format("2006-01-02") {
		return "released"
	}
	return "upcoming"
}

func tmdbSearchTool(ctx context.Context, query, mediaType string) (any, map[string]any, error) {
	apiKey := os.Getenv("TMDB_API_KEY")
	if apiKey == "" {
		return nil, nil, fmt.Errorf("TMDB_API_KEY not configured")
	}
	if query == "" {
		return nil, nil, fmt.Errorf("missing q")
	}
	endpoint := "movie"
	if mediaType == "show" || mediaType == "tv" {
		endpoint = "tv"
	}
	u := fmt.Sprintf("https://api.themoviedb.org/3/search/%s?api_key=%s&query=%s&page=1",
		endpoint, apiKey, url.QueryEscape(query))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var raw struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(body, &raw)
	out := []map[string]any{}
	for i, item := range raw.Results {
		if i >= 8 {
			break
		}
		title, _ := item["title"].(string)
		if title == "" {
			title, _ = item["name"].(string)
		}
		release, _ := item["release_date"].(string)
		if release == "" {
			release, _ = item["first_air_date"].(string)
		}
		year := ""
		if len(release) >= 4 {
			year = release[:4]
		}
		poster, _ := item["poster_path"].(string)
		if poster != "" {
			poster = "https://image.tmdb.org/t/p/w300" + poster
		}
		overview, _ := item["overview"].(string)
		var idRaw any = item["id"]
		out = append(out, map[string]any{
			"external_id":   fmt.Sprintf("tmdb:%s:%v", endpoint, idRaw),
			"title":         title,
			"year":          year,
			"release_date":  release,
			"release_state": tmdbReleaseState(release),
			"poster_url":    poster,
			"overview":      overview,
		})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func findFreeSlotsTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	date := argStr(args, "date")
	if date == "" {
		return nil, nil, fmt.Errorf("missing date")
	}
	dur := argInt(args, "duration_min", 60)
	earliest := argStr(args, "earliest")
	if earliest == "" {
		earliest = "09:00"
	}
	latest := argStr(args, "latest")
	if latest == "" {
		latest = "21:00"
	}
	dayStart, err := time.Parse("2006-01-02 15:04", date+" "+earliest)
	if err != nil {
		return nil, nil, fmt.Errorf("bad earliest: %w", err)
	}
	dayEnd, err := time.Parse("2006-01-02 15:04", date+" "+latest)
	if err != nil {
		return nil, nil, fmt.Errorf("bad latest: %w", err)
	}
	rows, err := d.QueryContext(ctx, `
		SELECT scheduled_at, COALESCE(duration_minutes,30), title FROM tasks
		WHERE user_id=$1 AND scheduled_at IS NOT NULL
		  AND scheduled_at::date = $2::date
		ORDER BY scheduled_at`, uid, date)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type block struct {
		start, end time.Time
		title      string
	}
	var busy []block
	for rows.Next() {
		var st time.Time
		var dm int
		var title string
		if err := rows.Scan(&st, &dm, &title); err == nil {
			busy = append(busy, block{start: st, end: st.Add(time.Duration(dm) * time.Minute), title: title})
		}
	}
	free := []map[string]any{}
	cur := dayStart
	for _, b := range busy {
		if b.start.After(cur) && int64(b.start.Sub(cur).Minutes()) >= dur {
			free = append(free, map[string]any{
				"start":        cur.Format("15:04"),
				"end":          b.start.Format("15:04"),
				"duration_min": int64(b.start.Sub(cur).Minutes()),
			})
		}
		if b.end.After(cur) {
			cur = b.end
		}
	}
	if dayEnd.After(cur) && int64(dayEnd.Sub(cur).Minutes()) >= dur {
		free = append(free, map[string]any{
			"start":        cur.Format("15:04"),
			"end":          dayEnd.Format("15:04"),
			"duration_min": int64(dayEnd.Sub(cur).Minutes()),
		})
	}
	return map[string]any{"date": date, "free_slots": free, "busy_count": len(busy)}, nil, nil
}

// ----- write handlers -----

// sanitizeAITaskEmails mirrors the REST sanitizeNotifyEmails guard (trim,
// RFC-validate, lowercase, de-dup, cap at 3) for the AI create path, which
// lives in a different package and can't reach the api helper.
func sanitizeAITaskEmails(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, e := range in {
		addr, err := mail.ParseAddress(strings.TrimSpace(e))
		if err != nil {
			continue
		}
		a := strings.ToLower(addr.Address)
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func createTaskTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	title := argStr(args, "title")
	if title == "" {
		return nil, nil, fmt.Errorf("missing title")
	}
	desc := argStr(args, "description")
	priority := argStr(args, "priority")
	if priority == "" {
		priority = "medium"
	}
	dueDate := argStr(args, "due_date")
	weekOf := argStr(args, "week_of")
	monthOf := argStr(args, "month_of")
	scheduled := argStr(args, "scheduled_at")
	dur := argInt(args, "duration_minutes", 30)
	important := argBool(args, "important", false)
	remind := argBool(args, "remind", false)

	var (
		id        int64
		dueArg    any = nil
		weekArg   any = nil
		monthArg  any = nil
		schArg    any = nil
		listArg   any = nil
		parentArg any = nil
	)
	if dueDate != "" {
		dueArg = dueDate
	}
	if weekOf != "" {
		weekArg = weekOf
	}
	if monthOf != "" {
		monthArg = monthOf
	}
	notifyJSON := "[]"
	if b, err := json.Marshal(sanitizeAITaskEmails(argStrSlice(args, "notify_emails"))); err == nil {
		notifyJSON = string(b)
	}
	if scheduled != "" {
		schArg = scheduled
		// Keep due_date aligned to the scheduled day (user's tz) when the model
		// set a time but no explicit date. Without this the task carries a
		// reminder yet has a NULL due_date, so it never surfaces in My Day /
		// Today / Planned — the "schedules well but timing feels off" glitch.
		if dueArg == nil {
			if t, err := time.Parse(time.RFC3339, scheduled); err == nil {
				dueArg = t.In(userTZLoc(ctx, d, uid)).Format("2006-01-02")
			}
		}
	}
	if lid := argInt(args, "list_id", 0); lid != 0 {
		// Validate list ownership.
		var ok int
		d.QueryRowContext(ctx, `SELECT 1 FROM task_lists WHERE id=$1 AND user_id=$2`, lid, uid).Scan(&ok)
		if ok == 1 {
			listArg = lid
		}
	}
	if pid := argInt(args, "parent_task_id", 0); pid != 0 {
		var ok int
		d.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id=$1 AND user_id=$2`, pid, uid).Scan(&ok)
		if ok == 1 {
			parentArg = pid
		}
	}

	// Steps: accept array of strings ("buy milk") or array of objects.
	var stepsJSON any = "[]"
	if raw, ok := args["steps"].([]any); ok && len(raw) > 0 {
		type stepRow struct {
			ID   string `json:"id"`
			Text string `json:"text"`
			Done bool   `json:"done"`
		}
		out := make([]stepRow, 0, len(raw))
		for i, item := range raw {
			text := ""
			done := false
			if s, ok := item.(string); ok {
				text = s
			} else if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					text = t
				}
				if d, ok := m["done"].(bool); ok {
					done = d
				}
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			out = append(out, stepRow{ID: fmt.Sprintf("s_%d_%d", time.Now().UnixNano(), i), Text: text, Done: done})
		}
		if b, err := json.Marshal(out); err == nil {
			stepsJSON = string(b)
		}
	}

	var scheduledAt sql.NullTime
	err := d.QueryRowContext(ctx, `
		INSERT INTO tasks (user_id, title, description, priority, status, due_date, week_of, month_of, scheduled_at, remind,
		                   notify_emails, duration_minutes, list_id, parent_task_id, important, steps)
		VALUES ($1,$2,$3,$4,'todo',$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13,$14,$15::jsonb)
		RETURNING id, scheduled_at`,
		uid, title, desc, priority, dueArg, weekArg, monthArg, schArg, remind, notifyJSON, dur, listArg, parentArg, important, stepsJSON,
	).Scan(&id, &scheduledAt)
	if err != nil {
		return nil, nil, err
	}
	if remind && scheduledAt.Valid {
		enqueueTaskReminder(ctx, id, scheduledAt.Time)
	}
	contentForTags := title + " " + desc
	for _, t := range argStrSlice(args, "tags") {
		contentForTags += " #" + t
	}
	if err := d.SyncTags(ctx, uid, "task", id, contentForTags); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "title": title},
		map[string]any{"kind": "task_created", "id": id, "title": title, "route": "/tasks"}, nil
}

// rescheduleTaskTool moves a task's due_date and/or scheduled_at. Mirrors the
// REST updateTask reschedule semantics: moving a past due day forward records
// a 'rescheduled' lifecycle row (so the Missed banner stops counting it) and
// an audit event; setting a time re-arms the reminder by clearing reminded_at.
func rescheduleTaskTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing id")
	}
	due := strings.TrimSpace(argStr(args, "due_date"))
	sched := strings.TrimSpace(argStr(args, "scheduled_at"))
	_, hasRemind := args["remind"]
	if due == "" && sched == "" && !hasRemind {
		return nil, nil, fmt.Errorf("nothing to change: pass due_date and/or scheduled_at")
	}

	var oldDue sql.NullString
	var status string
	if err := d.QueryRowContext(ctx, `SELECT due_date::text, status FROM tasks WHERE id=$1 AND user_id=$2`, id, uid).
		Scan(&oldDue, &status); err != nil {
		return nil, nil, fmt.Errorf("task not found")
	}

	loc := userTZLoc(ctx, d, uid)
	today := time.Now().In(loc).Format("2006-01-02")

	if due != "" {
		od := ""
		if oldDue.Valid {
			od = oldDue.String
		}
		// A move off an already-past day (while still open) is a reschedule,
		// not a miss — promote/insert the lifecycle row and log the event.
		if od != "" && od != due && status != "done" && status != "scratched" && od < today {
			var cnt int
			d.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_due_history WHERE user_id=$1 AND task_id=$2 AND due_date=$3`, uid, id, od).Scan(&cnt)
			if cnt == 0 {
				d.ExecContext(ctx, `INSERT INTO task_due_history (user_id, task_id, due_date, outcome) VALUES ($1,$2,$3,'rescheduled')`, uid, id, od)
			} else {
				d.ExecContext(ctx, `UPDATE task_due_history SET outcome='rescheduled' WHERE user_id=$1 AND task_id=$2 AND due_date=$3`, uid, id, od)
			}
			d.ExecContext(ctx, `INSERT INTO task_events (user_id, task_id, kind, from_val, to_val) VALUES ($1,$2,'rescheduled',$3,$4)`, uid, id, od, due)
		}
		d.ExecContext(ctx, `UPDATE tasks SET due_date=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3`, due, id, uid)
		// Day/week scope are exclusive — rescheduling to a concrete day converts
		// a week task to a day task, so clear any stale week_of.
		d.ExecContext(ctx, `UPDATE tasks SET week_of=NULL WHERE id=$1 AND user_id=$2`, id, uid)
	}

	if sched != "" {
		d.ExecContext(ctx, `UPDATE tasks SET scheduled_at=$1, reminded_at=NULL, updated_at=NOW() WHERE id=$2 AND user_id=$3`, sched, id, uid)
		if due == "" {
			if t, err := time.Parse(time.RFC3339, sched); err == nil {
				d.ExecContext(ctx, `UPDATE tasks SET due_date=$1 WHERE id=$2 AND user_id=$3`, t.In(loc).Format("2006-01-02"), id, uid)
			}
		}
	}

	if hasRemind {
		d.ExecContext(ctx, `UPDATE tasks SET remind=$1, reminded_at=NULL, updated_at=NOW() WHERE id=$2 AND user_id=$3`, argBool(args, "remind", false), id, uid)
	}
	if sched != "" || hasRemind {
		enqueueTaskReminderFromDB(ctx, d, uid, int64(id))
	}

	return map[string]any{"id": id, "due_date": due, "scheduled_at": sched},
		map[string]any{"kind": "task_updated", "id": id, "route": "/tasks"}, nil
}

// scratchTaskTool flips a task to (or back from) the 'scratched' status — an
// abandoned task kept for the record but excluded from open lists, smart
// views, and the reminder cron. Reversible via unscratch.
func scratchTaskTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing id")
	}
	var cur string
	if err := d.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=$1 AND user_id=$2`, id, uid).Scan(&cur); err != nil {
		return nil, nil, fmt.Errorf("task not found")
	}
	next := "scratched"
	if argBool(args, "unscratch", false) {
		next = "todo"
	}
	if cur != next {
		if _, err := d.ExecContext(ctx, `UPDATE tasks SET status=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3`, next, id, uid); err != nil {
			return nil, nil, err
		}
		d.ExecContext(ctx, `INSERT INTO task_events (user_id, task_id, kind, from_val, to_val) VALUES ($1,$2,'status',$3,$4)`, uid, id, cur, next)
	}
	return map[string]any{"id": id, "status": next},
		map[string]any{"kind": "task_updated", "id": id, "route": "/tasks"}, nil
}

func enqueueTaskReminderFromDB(ctx context.Context, d *db.DB, uid string, id int64) {
	var scheduledAt time.Time
	err := d.QueryRowContext(ctx, `
		SELECT scheduled_at
		  FROM tasks
		 WHERE id = $1
		   AND user_id = $2
		   AND remind = TRUE
		   AND reminded_at IS NULL
		   AND scheduled_at IS NOT NULL
		   AND status NOT IN ('done','scratched')`,
		id, uid).Scan(&scheduledAt)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		log.Warn().Err(err).Int64("task", id).Msg("AI reminder enqueue lookup failed")
		return
	}
	enqueueTaskReminder(ctx, id, scheduledAt)
}

func enqueueTaskReminder(ctx context.Context, id int64, scheduledAt time.Time) {
	if err := reminderqueue.EnqueueTask(ctx, id, scheduledAt); err != nil {
		log.Warn().Err(err).Int64("task", id).Msg("AI reminder cloud task enqueue failed")
	}
}

func enqueueMultiReminder(ctx context.Context, id int64, remindAt time.Time) {
	if err := reminderqueue.EnqueueMulti(ctx, id, remindAt); err != nil {
		log.Warn().Err(err).Int64("reminder", id).Msg("AI task_reminder cloud task enqueue failed")
	}
}

// updateTaskTool edits a task's mutable fields in place. Only the fields the
// model actually passes are touched; everything else stays as-is. This is the
// AI's path to move/promote a task between lists (list_id / to_inbox) — the gap
// the read/write tool split previously had no cover for. Time/reminder edits
// stay with reschedule_task, which owns the reschedule lifecycle bookkeeping.
func updateTaskTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing id")
	}
	var exists int
	d.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id=$1 AND user_id=$2`, id, uid).Scan(&exists)
	if exists != 1 {
		return nil, nil, fmt.Errorf("task not found")
	}

	var (
		sets []string
		vals []any
	)
	add := func(frag string, v any) {
		vals = append(vals, v)
		sets = append(sets, fmt.Sprintf("%s=$%d", frag, len(vals)))
	}

	var (
		titleUpdated = false
		descUpdated  = false
	)
	if _, ok := args["title"]; ok {
		if t := strings.TrimSpace(argStr(args, "title")); t != "" {
			add("title", t)
			titleUpdated = true
		}
	}
	if _, ok := args["description"]; ok {
		add("description", argStr(args, "description"))
		descUpdated = true
	}
	if _, ok := args["priority"]; ok {
		if p := strings.TrimSpace(argStr(args, "priority")); p == "high" || p == "medium" || p == "low" {
			add("priority", p)
		}
	}
	if _, ok := args["due_date"]; ok {
		if due := strings.TrimSpace(argStr(args, "due_date")); due != "" {
			add("due_date", due)
			// Day/week/month scope are exclusive — a day clears week + month.
			sets = append(sets, "week_of=NULL", "month_of=NULL")
		}
	}
	if lid := argInt(args, "list_id", 0); lid != 0 {
		var ok int
		d.QueryRowContext(ctx, `SELECT 1 FROM task_lists WHERE id=$1 AND user_id=$2`, lid, uid).Scan(&ok)
		if ok != 1 {
			return nil, nil, fmt.Errorf("list not found")
		}
		add("list_id", lid)
	} else if argBool(args, "to_inbox", false) {
		add("list_id", nil)
	}
	if pid := argInt(args, "parent_task_id", 0); pid != 0 {
		var ok int
		d.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id=$1 AND user_id=$2`, pid, uid).Scan(&ok)
		if ok != 1 {
			return nil, nil, fmt.Errorf("parent task not found")
		}
		add("parent_task_id", pid)
	}

	if len(sets) == 0 {
		return nil, nil, fmt.Errorf("nothing to update: pass at least one of title, description, priority, due_date, list_id, parent_task_id, to_inbox")
	}

	vals = append(vals, id, uid)
	q := fmt.Sprintf(`UPDATE tasks SET %s, updated_at=NOW() WHERE id=$%d AND user_id=$%d`,
		strings.Join(sets, ", "), len(vals)-1, len(vals))
	if _, err := d.ExecContext(ctx, q, vals...); err != nil {
		return nil, nil, err
	}
	if titleUpdated || descUpdated {
		var finalTitle, finalDesc string
		if err := d.QueryRowContext(ctx, `SELECT title, description FROM tasks WHERE id=$1 AND user_id=$2`, id, uid).Scan(&finalTitle, &finalDesc); err == nil {
			contentForTags := finalTitle + " " + finalDesc
			_ = d.SyncTags(ctx, uid, "task", id, contentForTags)
		}
	}
	return map[string]any{"id": id, "updated": true},
		map[string]any{"kind": "task_updated", "id": id, "route": "/tasks"}, nil
}

func createHabitTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	name := argStr(args, "name")
	if name == "" {
		return nil, nil, fmt.Errorf("missing name")
	}
	freq := argStr(args, "frequency")
	if freq == "" {
		freq = "daily"
	}
	color := argStr(args, "color")
	if color == "" {
		color = "#2D5A4F"
	}
	var id int64
	err := d.QueryRowContext(ctx, `INSERT INTO habits (user_id, name, frequency, color) VALUES ($1,$2,$3,$4) RETURNING id`,
		uid, name, freq, color).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "name": name},
		map[string]any{"kind": "habit_created", "id": id, "title": name, "route": "/habits"}, nil
}

func createMemoTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	content := argStr(args, "content")
	if content == "" {
		return nil, nil, fmt.Errorf("missing content")
	}
	pinned := argBool(args, "pinned", false)
	var id int64
	err := d.QueryRowContext(ctx, `INSERT INTO memos (user_id, content, pinned) VALUES ($1,$2,$3) RETURNING id`,
		uid, content, pinned).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	for _, tag := range argStrSlice(args, "tags") {
		d.ExecContext(ctx, `INSERT INTO tags (user_id, entity_type, entity_id, tag) VALUES ($1,'memo',$2,$3)`, uid, id, tag)
	}
	preview := content
	if len(preview) > 60 {
		preview = preview[:60] + "…"
	}
	return map[string]any{"id": id},
		map[string]any{"kind": "memo_created", "id": id, "title": preview, "route": "/"}, nil
}

func createJournalTool(ctx context.Context, d *db.DB, store storage.Storage, uid string, args map[string]any) (any, map[string]any, error) {
	content := argStr(args, "content")
	if content == "" {
		return nil, nil, fmt.Errorf("missing content")
	}
	date := argStr(args, "date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	mood := argStr(args, "mood")
	locLabel := argStr(args, "location_label")
	blobKey := fmt.Sprintf("user_%s/journal/%s.md", uid, date)
	if err := store.Put(ctx, blobKey, []byte(content), "text/markdown"); err != nil {
		return nil, nil, err
	}
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO journal_entries (user_id, date, blob_key, mood, location_label) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (user_id, date) DO UPDATE SET blob_key=EXCLUDED.blob_key, mood=EXCLUDED.mood,
		  location_label=EXCLUDED.location_label, updated_at=NOW()
		RETURNING id`, uid, date, blobKey, nullableStr(mood), locLabel).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "date": date},
		map[string]any{"kind": "journal_created", "id": id, "title": date, "route": "/journal?date=" + date}, nil
}

func normalizeAINoteFolder(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		seg = strings.ReplaceAll(seg, "\\", "_")
		out = append(out, seg)
	}
	return strings.Join(out, "/")
}

func deriveNoteTitle(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "#")
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 80 {
				line = line[:80]
			}
			return line
		}
	}
	return "Untitled"
}

func createNoteTool(ctx context.Context, d *db.DB, store storage.Storage, uid string, args map[string]any) (any, map[string]any, error) {
	content := argStr(args, "content")
	title := strings.TrimSpace(argStr(args, "title"))
	if title == "" {
		title = deriveNoteTitle(content)
	}
	folder := normalizeAINoteFolder(argStr(args, "folder"))

	safe := strings.ReplaceAll(title, "/", "_")
	safe = strings.ReplaceAll(safe, "\\", "_")
	if safe == "" {
		safe = "untitled"
	}
	parts := []string{fmt.Sprintf("user_%s", uid), "notes"}
	if folder != "" {
		parts = append(parts, folder)
	}
	parts = append(parts, safe+".md")
	blobKey := strings.Join(parts, "/")

	if err := store.Put(ctx, blobKey, []byte(content), "text/markdown"); err != nil {
		return nil, nil, err
	}
	var id int64
	err := d.QueryRowContext(ctx,
		`INSERT INTO notes (user_id, title, blob_key, folder, description) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		uid, title, blobKey, folder, argStr(args, "description")).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	for _, tag := range argStrSlice(args, "tags") {
		d.ExecContext(ctx, `INSERT INTO tags (user_id, entity_type, entity_id, tag) VALUES ($1,'note',$2,$3)`, uid, id, tag)
	}
	return map[string]any{"id": id, "title": title, "folder": folder},
		map[string]any{"kind": "note_created", "id": id, "title": title, "route": fmt.Sprintf("/notes?id=%d", id)}, nil
}

func createFolderTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	path := normalizeAINoteFolder(argStr(args, "path"))
	if path == "" {
		return nil, nil, fmt.Errorf("missing path")
	}
	_, err := d.ExecContext(ctx,
		`INSERT INTO note_folders (user_id, path) VALUES ($1, $2) ON CONFLICT DO NOTHING`, uid, path)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"path": path},
		map[string]any{"kind": "folder_created", "title": path, "route": "/notes"}, nil
}

func addMediaTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	title := argStr(args, "title")
	mtype := argStr(args, "type")
	if title == "" || mtype == "" {
		return nil, nil, fmt.Errorf("missing title or type")
	}
	status := argStr(args, "status")
	statusSpecified := status != ""
	if status == "" {
		status = "pending"
	}
	year := argInt(args, "year", 0)
	var yearArg any
	if year > 0 {
		yearArg = year
	}
	rating := argInt(args, "rating", 0)
	var ratingArg any
	if rating >= 1 && rating <= 5 {
		ratingArg = rating
	}
	// Dedupe — if the user already has the same title+type, update its
	// status instead of inserting a duplicate. Lite tier sometimes
	// triggers add_media twice for the same title in one round.
	var existingID int64
	d.QueryRowContext(ctx,
		`SELECT id FROM media WHERE user_id=$1 AND LOWER(title)=LOWER($2) AND type=$3 LIMIT 1`,
		uid, title, mtype).Scan(&existingID)
	if existingID > 0 {
		_, err := d.ExecContext(ctx,
			`UPDATE media SET status=$1, rating=COALESCE($2, rating), updated_at=NOW()
			 WHERE id=$3 AND user_id=$4`,
			status, ratingArg, existingID, uid)
		if err != nil {
			return nil, nil, err
		}
		return map[string]any{"id": existingID, "title": title, "updated": true},
			map[string]any{"kind": "media_added", "id": existingID, "title": title, "route": "/media"}, nil
	}

	// Metadata: when the model didn't supply an external_id, route through
	// the same enrichment the manual Add flow uses (TMDB / Open Library) so
	// AI-added titles get poster, year, genre, and (for shows) season data.
	extID := argStr(args, "external_id")
	genre := argStr(args, "genre")
	poster := argStr(args, "poster_url")
	releaseDate := normalizeReleaseDate(argStr(args, "release_date"))
	var seasonsTotal, episodesTotal int
	var seasonEpisodes []int
	if extID == "" {
		meta := enrichMediaMeta(ctx, title, mtype)
		extID = meta.ExternalID
		if poster == "" {
			poster = meta.PosterURL
		}
		if genre == "" {
			genre = meta.Genre
		}
		if releaseDate == "" {
			releaseDate = meta.ReleaseDate
		}
		if year == 0 && meta.Year > 0 {
			year = int64(meta.Year)
			yearArg = year
		}
		seasonsTotal, episodesTotal, seasonEpisodes = meta.SeasonsTotal, meta.EpisodesTotal, meta.SeasonEpisodes
	}
	seJSON := []byte("[]")
	if len(seasonEpisodes) > 0 {
		if b, err := json.Marshal(seasonEpisodes); err == nil {
			seJSON = b
		}
	}
	var releaseDateArg any
	if releaseDate != "" {
		releaseDateArg = releaseDate
		if !statusSpecified || status == "pending" {
			nowStr := time.Now().Format("2006-01-02")
			cleanDate := strings.TrimSpace(releaseDate)
			if len(cleanDate) >= 10 {
				cleanDate = cleanDate[:10]
				if _, err := time.Parse("2006-01-02", cleanDate); err == nil && cleanDate > nowStr {
					status = "upcoming"
				}
			}
		}
	}

	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO media (user_id, title, type, status, external_id, year, genre, poster_url, rating,
		                   release_date, seasons_total, episodes_total, season_episodes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb) RETURNING id`,
		uid, title, mtype, status,
		extID, yearArg, genre, poster, ratingArg,
		releaseDateArg, seasonsTotal, episodesTotal, string(seJSON)).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "title": title},
		map[string]any{"kind": "media_added", "id": id, "title": title, "route": "/media"}, nil
}

func createTxnTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	acct := argInt(args, "account_id", 0)
	if acct == 0 {
		return nil, nil, fmt.Errorf("missing account_id")
	}
	ttype := argStr(args, "type")
	if ttype == "" {
		ttype = "expense"
	}
	amount := argFloat(args, "amount")
	if amount <= 0 {
		return nil, nil, fmt.Errorf("amount must be positive")
	}
	desc := argStr(args, "description")
	date := argStr(args, "date")

	// Resolve Category
	var catID int64 = argInt(args, "category_id", 0)
	catName := strings.TrimSpace(argStr(args, "category_name"))

	if catID == 0 && catName != "" {
		// 1. Try exact case-insensitive match
		err := d.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND LOWER(name) = LOWER($2) LIMIT 1`, uid, catName).Scan(&catID)
		if err != nil || catID == 0 {
			// 2. Try substring match
			d.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND name ILIKE $2 LIMIT 1`, uid, "%"+catName+"%").Scan(&catID)
		}
		if catID == 0 {
			// 3. Fallback to default "Other" or "Others"
			d.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND (LOWER(name) = 'other' OR LOWER(name) = 'others') LIMIT 1`, uid).Scan(&catID)
		}
	}

	var catArg any = nil
	if catID > 0 {
		catArg = catID
	}

	// txn_at: a model-supplied date is read as IST midnight; absent → now (so a
	// chat-logged "spent 200 on lunch" gets a real time, not midnight).
	insArgs := []any{uid, acct, catArg, ttype, amount, desc}
	txnCol := "NOW()"
	if date != "" {
		txnCol = "($7::timestamp AT TIME ZONE 'Asia/Kolkata')"
		insArgs = append(insArgs, date)
	}
	var id int64
	err := d.QueryRowContext(ctx,
		`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at)
		 VALUES ($1,$2,$3,$4,$5,$6,`+txnCol+`) RETURNING id`,
		insArgs...).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "amount": amount, "type": ttype},
		map[string]any{"kind": "transaction_created", "id": id, "title": fmt.Sprintf("%s %.2f", ttype, amount), "route": "/finance/transactions"}, nil
}

// resolveCategoryID maps a category_name to an existing category id using
// the same exact→substring→Other fallback as createTxnTool. Returns 0 when
// nothing matches and no Other category exists.
func resolveCategoryID(ctx context.Context, d *db.DB, uid, name string) int64 {
	var catID int64
	if err := d.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND LOWER(name) = LOWER($2) LIMIT 1`, uid, name).Scan(&catID); err != nil || catID == 0 {
		d.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND name ILIKE $2 LIMIT 1`, uid, "%"+name+"%").Scan(&catID)
	}
	if catID == 0 {
		d.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND (LOWER(name) = 'other' OR LOWER(name) = 'others') LIMIT 1`, uid).Scan(&catID)
	}
	return catID
}

// updateTxnTool mutates an existing transaction. Only the fields the model
// passes are touched; category_name is resolved to an id server-side.
func updateTxnTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing id")
	}
	var owned int
	d.QueryRowContext(ctx, `SELECT 1 FROM fin_transactions WHERE id=$1 AND user_id=$2`, id, uid).Scan(&owned)
	if owned != 1 {
		return nil, nil, fmt.Errorf("transaction not found")
	}

	sets := []string{"updated_at = NOW()"}
	vals := []any{}
	ph := 1
	add := func(col string, v any) {
		sets = append(sets, fmt.Sprintf("%s = $%d", col, ph))
		vals = append(vals, v)
		ph++
	}

	if acctID := argInt(args, "account_id", 0); acctID > 0 {
		add("account_id", acctID)
	}
	catID := argInt(args, "category_id", 0)
	if catID == 0 {
		if catName := strings.TrimSpace(argStr(args, "category_name")); catName != "" {
			catID = resolveCategoryID(ctx, d, uid, catName)
		}
	}
	if catID > 0 {
		add("category_id", catID)
	}
	if t := argStr(args, "type"); t == "expense" || t == "income" {
		add("type", t)
	}
	if amt := argFloat(args, "amount"); amt > 0 {
		add("amount", amt)
	}
	if v, ok := args["description"]; ok && v != nil {
		add("description", argStr(args, "description"))
	}
	if dt := argStr(args, "date"); dt != "" {
		sets = append(sets, fmt.Sprintf("txn_at = ($%d::timestamp AT TIME ZONE 'Asia/Kolkata')", ph))
		vals = append(vals, dt)
		ph++
	}
	if len(sets) == 1 {
		return nil, nil, fmt.Errorf("nothing to update")
	}

	vals = append(vals, id, uid)
	q := "UPDATE fin_transactions SET " + strings.Join(sets, ", ") +
		fmt.Sprintf(" WHERE id=$%d AND user_id=$%d", ph, ph+1)
	if _, err := d.ExecContext(ctx, q, vals...); err != nil {
		return nil, nil, err
	}
	// Keep a transfer pair's back-reference in sync when the account moves.
	if acctID := argInt(args, "account_id", 0); acctID > 0 {
		d.ExecContext(ctx, "UPDATE fin_transactions SET linked_account=$1, updated_at=NOW() WHERE transfer_pair=$2 AND user_id=$3", acctID, id, uid)
	}
	return map[string]any{"id": id, "updated": true},
		map[string]any{"kind": "transaction_updated", "id": id, "route": "/finance/transactions"}, nil
}

func listCategoriesTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	kind := argStr(args, "kind")
	clauses := []string{"user_id=$1"}
	vals := []any{uid}
	if kind != "" {
		clauses = append(clauses, "kind=$2")
		vals = append(vals, kind)
	}
	q := `SELECT id, name, kind, color, icon FROM fin_categories WHERE ` +
		strings.Join(clauses, " AND ") + ` ORDER BY kind, name`
	rows, err := d.QueryContext(ctx, q, vals...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, k, color, icon string
		rows.Scan(&id, &name, &k, &color, &icon)
		out = append(out, map[string]any{
			"id": id, "name": name, "kind": k, "color": color, "icon": icon,
		})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func listBudgetsTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	limit := argInt(args, "limit", 10)
	rows, err := d.QueryContext(ctx, `SELECT id, name, period, start_date::text, end_date::text, total_amount 
		FROM fin_budgets WHERE user_id = $1 ORDER BY start_date DESC LIMIT $2`, uid, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, period, startDate, endDate string
		var totalAmount float64
		rows.Scan(&id, &name, &period, &startDate, &endDate, &totalAmount)

		// Fetch items breakdown
		items := []map[string]any{}
		itemRows, _ := d.QueryContext(ctx, `
			SELECT i.id, i.category_id, i.amount, COALESCE(c.name, 'Other')
			FROM fin_budget_items i
			LEFT JOIN fin_categories c ON c.id = i.category_id
			WHERE i.budget_id = $1`, id)
		if itemRows != nil {
			for itemRows.Next() {
				var itemID int64
				var catID sql.NullInt64
				var amt float64
				var catName string
				itemRows.Scan(&itemID, &catID, &amt, &catName)
				var spent float64
				if catID.Valid {
					d.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount),0) FROM fin_transactions
						WHERE user_id = $1 AND type = 'expense' AND category_id = $2 AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $3 AND $4`,
						uid, catID.Int64, startDate, endDate).Scan(&spent)
				}
				item := map[string]any{
					"id": itemID, "category_name": catName, "amount": amt, "spent": spent,
				}
				if catID.Valid {
					item["category_id"] = catID.Int64
				}
				items = append(items, item)
			}
			itemRows.Close()
		}

		var totalSpent float64
		d.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND type = 'expense' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3`,
			uid, startDate, endDate).Scan(&totalSpent)

		out = append(out, map[string]any{
			"id": id, "name": name, "period": period, "start_date": startDate,
			"end_date": endDate, "total_amount": totalAmount, "spent": totalSpent,
			"items": items,
		})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}
