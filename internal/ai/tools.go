package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/genai"

	"sajni/internal/db"
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
	Handler     func(ctx context.Context, uid int64, args map[string]any) (data any, meta map[string]any, err error)
}

// dispatch looks up a tool by name and invokes it. Unknown tools and
// bad-arg errors come back as structured errors so the model can recover.
func (s *Service) dispatch(ctx context.Context, uid int64, name string, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return getCurrentContextTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_tasks",
			Description: "List the user's tasks with optional filters. Use this before suggesting actions on tasks. Smart filters: 'my_day' (due today), 'important' (starred), 'planned' (has due date), 'inbox' (no list).",
			Schema: obj(map[string]*genai.Schema{
				"smart":     str("Smart-list shortcut: 'my_day' | 'important' | 'planned' | 'inbox' | 'all'."),
				"list_id":   intg("Filter to a specific user list."),
				"parent_id": intg("Set to non-zero to fetch children of a parent task. Default returns top-level only."),
				"status":    str("Filter by status: 'todo', 'done', 'in_progress'."),
				"due_from":  str("Lower bound (inclusive) ISO date YYYY-MM-DD."),
				"due_to":    str("Upper bound (inclusive) ISO date YYYY-MM-DD."),
				"priority":  str("Filter by priority: 'high', 'medium', 'low'."),
				"important": boolean("Restrict to starred tasks."),
				"limit":     intg("Maximum tasks to return (default 50)."),
			}),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return listTasksTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "list_task_lists",
			Description: "List the user's custom task lists (groupings) with their open-task counts. Use to disambiguate when a request mentions a list by name (e.g. 'add to my Work list').",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return listTaskListsTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_habits",
			Description: "List the user's habits with done-today flag and total log count.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return listJournalTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "get_journal_entry",
			Description: "Read the full content of one journal entry by date.",
			Schema: obj(map[string]*genai.Schema{
				"date": str("ISO date YYYY-MM-DD."),
			}, "date"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return listMediaTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "media_taste",
			Description: "Returns the user's taste profile: top-rated titles, favourite genres (weighted by rating), completion vs. drop ratio per type, and most-recently completed entries. Call this once BEFORE tmdb_search when recommending anything so the suggestion is personalised, not generic.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return mediaTasteTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_finance_accounts",
			Description: "List finance accounts with current computed balance.",
			Schema:      obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return listAccountsTool(ctx, d, uid)
			},
		},
		{
			Name:        "list_finance_transactions",
			Description: "List finance transactions with filters.",
			Schema: obj(map[string]*genai.Schema{
				"account_id": intg("Filter by account."),
				"date_from":  str("ISO date."),
				"date_to":    str("ISO date."),
				"limit":      intg("Default 50."),
			}),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return listTxnsTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "search",
			Description: "Free-text search across memos, tasks, notes, journals, habits, media. Use for 'find anything about X' style questions.",
			Schema: obj(map[string]*genai.Schema{
				"q":     str("Search query."),
				"types": arrayOf(str(""), "Optional whitelist of types: memo, task, note, journal, habit, media."),
			}, "q"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return runSearchTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "tmdb_search",
			Description: "Search TMDB for real movie or show metadata (title, year, poster, overview). Use this before recommending media so the suggestion comes with real data and an external_id you can pass to add_media.",
			Schema: obj(map[string]*genai.Schema{
				"q":    str("Movie or show title to search."),
				"type": str("'movie' or 'show'. Defaults to movie."),
			}, "q"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return findFreeSlotsTool(ctx, d, uid, args)
			},
		},

		// ---------------- WRITE ----------------
		{
			Name:        "create_task",
			Description: "Create a new task. Resolve relative dates ('tomorrow', 'next monday') against get_current_context first. Use list_task_lists to look up list_id by name; use list_tasks to find a parent_task_id when nesting.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"title":            str("Required. Short title."),
				"description":      str("Optional details."),
				"priority":         str("'high' | 'medium' | 'low'. Default 'medium'."),
				"due_date":         str("Optional ISO date YYYY-MM-DD."),
				"scheduled_at":     str("Optional ISO timestamp YYYY-MM-DDTHH:MM:00Z."),
				"duration_minutes": intg("Optional. Default 30."),
				"list_id":          intg("Optional. Place the task inside this user list."),
				"parent_task_id":   intg("Optional. Make this a subtask of the given task id."),
				"important":        boolean("Optional. Star the task (Important smart list)."),
				"steps":            arrayOf(str(""), "Optional inline checklist (array of step text strings)."),
				"tags":             arrayOf(str(""), "Optional tag list."),
			}, "title"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return createTaskListTool(ctx, d, uid, args)
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				if id == 0 {
					return nil, nil, fmt.Errorf("missing id")
				}
				_, err := d.Exec(`UPDATE tasks SET status='done', updated_at=NOW() WHERE id=$1 AND user_id=$2`, id, uid)
				if err != nil {
					return nil, nil, err
				}
				return map[string]any{"id": id, "status": "done"},
					map[string]any{"kind": "task_completed", "id": id}, nil
			},
		},
		{
			Name:        "delete_task",
			Description: "Delete a task permanently.",
			Mutating:    true,
			Schema:      obj(map[string]*genai.Schema{"id": intg("Task id.")}, "id"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Name:        "create_habit",
			Description: "Create a new habit to track.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"name":      str("Required."),
				"frequency": str("'daily' | 'weekly'. Default 'daily'."),
				"color":     str("Hex like '#2D5A4F'."),
			}, "name"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
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
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return createMemoTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "create_journal_entry",
			Description: "Create or replace a journal entry for the given date. Content is stored as a markdown blob.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"date":    str("ISO date. Defaults to today."),
				"mood":    str("e.g. 'happy', 'focused', 'tired'."),
				"content": str("The entry body in markdown."),
			}, "content"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return createJournalTool(ctx, d, store, uid, args)
			},
		},
		{
			Name:        "add_media",
			Description: "Add a movie / show / book to the user's library. If you have an external_id from tmdb_search, pass it along with poster_url, year, etc. so the entry is fully populated.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"title":       str("Required."),
				"type":        str("'movie' | 'show' | 'book'."),
				"status":      str("'pending' | 'watching' | 'done'. Default 'pending'."),
				"external_id": str("Optional TMDB external id from tmdb_search."),
				"year":        intg("Optional release year."),
				"genre":       str("Optional."),
				"poster_url":  str("Optional."),
			}, "title", "type"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return addMediaTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "create_transaction",
			Description: "Record a finance transaction against an existing account. Use list_finance_accounts first to get account_id.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"account_id":  intg("Required."),
				"type":        str("'expense' | 'income'. Default 'expense'."),
				"amount":      num("Positive amount."),
				"description": str("What it was for."),
				"date":        str("ISO date. Defaults to today."),
			}, "account_id", "amount"),
			Handler: func(ctx context.Context, uid int64, args map[string]any) (any, map[string]any, error) {
				return createTxnTool(ctx, d, uid, args)
			},
		},
	}
}

// ----- handler implementations -----

func getCurrentContextTool(ctx context.Context, d *db.DB, uid int64) (any, map[string]any, error) {
	now := time.Now()
	today := now.Format("2006-01-02")
	out := map[string]any{
		"today":     today,
		"weekday":   now.Weekday().String(),
		"timestamp": now.Format(time.RFC3339),
	}
	var openTasks, dueToday, habitsTotal, habitsDone, memos7d int
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status<>'done'`, uid).Scan(&openTasks)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1 AND status<>'done' AND due_date=$2`, uid, today).Scan(&dueToday)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM habits WHERE user_id=$1`, uid).Scan(&habitsTotal)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM habit_logs WHERE user_id=$1 AND logged_date=$2`, uid, today).Scan(&habitsDone)
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM memos WHERE user_id=$1 AND created_at > NOW() - INTERVAL '7 days'`, uid).Scan(&memos7d)
	out["open_tasks"] = openTasks
	out["tasks_due_today"] = dueToday
	out["habits"] = map[string]any{"total": habitsTotal, "done_today": habitsDone}
	out["recent_memos_7d"] = memos7d
	return out, nil, nil
}

func listTasksTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
	clauses := []string{"user_id=$1"}
	vals := []any{uid}

	switch argStr(args, "smart") {
	case "my_day":
		clauses = append(clauses, "due_date = CURRENT_DATE", "status != 'done'")
	case "important":
		clauses = append(clauses, "important = TRUE", "status != 'done'")
	case "planned":
		clauses = append(clauses, "due_date IS NOT NULL", "status != 'done'")
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

func listTaskListsTool(ctx context.Context, d *db.DB, uid int64) (any, map[string]any, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT l.id, l.name, l.color, l.icon,
		       COALESCE(c.cnt, 0)
		FROM task_lists l
		LEFT JOIN (
			SELECT list_id, COUNT(*) AS cnt FROM tasks
			WHERE user_id=$1 AND parent_task_id IS NULL AND status != 'done'
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

func createTaskListTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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

func listHabitsTool(ctx context.Context, d *db.DB, uid int64) (any, map[string]any, error) {
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

func listJournalTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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

func getJournalTool(ctx context.Context, d *db.DB, store storage.Storage, uid int64, date string) (any, map[string]any, error) {
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

func listMemosTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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
func mediaTasteTool(ctx context.Context, d *db.DB, uid int64) (any, map[string]any, error) {
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

func listMediaTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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

func listAccountsTool(ctx context.Context, d *db.DB, uid int64) (any, map[string]any, error) {
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

func listTxnsTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
	clauses := []string{"user_id=$1"}
	vals := []any{uid}
	if a := argInt(args, "account_id", 0); a > 0 {
		clauses = append(clauses, fmt.Sprintf("account_id=$%d", len(vals)+1))
		vals = append(vals, a)
	}
	if df := argStr(args, "date_from"); df != "" {
		clauses = append(clauses, fmt.Sprintf("txn_date >= $%d", len(vals)+1))
		vals = append(vals, df)
	}
	if dt := argStr(args, "date_to"); dt != "" {
		clauses = append(clauses, fmt.Sprintf("txn_date <= $%d", len(vals)+1))
		vals = append(vals, dt)
	}
	limit := argInt(args, "limit", 50)
	q := `SELECT id, account_id, type, amount, description, txn_date::text FROM fin_transactions WHERE ` +
		strings.Join(clauses, " AND ") +
		fmt.Sprintf(` ORDER BY txn_date DESC LIMIT %d`, limit)
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
		rows.Scan(&id, &acct, &ttype, &amount, &desc, &date)
		out = append(out, map[string]any{"id": id, "account_id": acct, "type": ttype, "amount": amount, "description": desc, "date": date})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func runSearchTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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
	return map[string]any{"items": out, "count": len(out)}, nil, nil
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
			"external_id": fmt.Sprintf("tmdb:%s:%v", endpoint, idRaw),
			"title":       title,
			"year":        year,
			"poster_url":  poster,
			"overview":    overview,
		})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func findFreeSlotsTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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

func createTaskTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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
	scheduled := argStr(args, "scheduled_at")
	dur := argInt(args, "duration_minutes", 30)
	important := argBool(args, "important", false)

	var (
		id        int64
		dueArg    any = nil
		schArg    any = nil
		listArg   any = nil
		parentArg any = nil
	)
	if dueDate != "" {
		dueArg = dueDate
	}
	if scheduled != "" {
		schArg = scheduled
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

	err := d.QueryRowContext(ctx, `
		INSERT INTO tasks (user_id, title, description, priority, status, due_date, scheduled_at,
		                   duration_minutes, list_id, parent_task_id, important, steps)
		VALUES ($1,$2,$3,$4,'todo',$5,$6,$7,$8,$9,$10,$11::jsonb)
		RETURNING id`,
		uid, title, desc, priority, dueArg, schArg, dur, listArg, parentArg, important, stepsJSON,
	).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	for _, tag := range argStrSlice(args, "tags") {
		d.ExecContext(ctx, `INSERT INTO tags (user_id, entity_type, entity_id, tag) VALUES ($1,'task',$2,$3)`, uid, id, tag)
	}
	return map[string]any{"id": id, "title": title},
		map[string]any{"kind": "task_created", "id": id, "title": title, "route": "/tasks"}, nil
}

func createHabitTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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

func createMemoTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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

func createJournalTool(ctx context.Context, d *db.DB, store storage.Storage, uid int64, args map[string]any) (any, map[string]any, error) {
	content := argStr(args, "content")
	if content == "" {
		return nil, nil, fmt.Errorf("missing content")
	}
	date := argStr(args, "date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	mood := argStr(args, "mood")
	blobKey := fmt.Sprintf("user_%d/journal/%s.md", uid, date)
	if err := store.Put(ctx, blobKey, []byte(content), "text/markdown"); err != nil {
		return nil, nil, err
	}
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO journal_entries (user_id, date, blob_key, mood) VALUES ($1,$2,$3,$4)
		ON CONFLICT (user_id, date) DO UPDATE SET blob_key=EXCLUDED.blob_key, mood=EXCLUDED.mood, updated_at=NOW()
		RETURNING id`, uid, date, blobKey, nullableStr(mood)).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "date": date},
		map[string]any{"kind": "journal_created", "id": id, "title": date, "route": "/journal?date=" + date}, nil
}

func addMediaTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
	title := argStr(args, "title")
	mtype := argStr(args, "type")
	if title == "" || mtype == "" {
		return nil, nil, fmt.Errorf("missing title or type")
	}
	status := argStr(args, "status")
	if status == "" {
		status = "pending"
	}
	year := argInt(args, "year", 0)
	var yearArg any
	if year > 0 {
		yearArg = year
	}
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO media (user_id, title, type, status, external_id, year, genre, poster_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		uid, title, mtype, status,
		argStr(args, "external_id"), yearArg, argStr(args, "genre"), argStr(args, "poster_url")).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "title": title},
		map[string]any{"kind": "media_added", "id": id, "title": title, "route": "/media"}, nil
}

func createTxnTool(ctx context.Context, d *db.DB, uid int64, args map[string]any) (any, map[string]any, error) {
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
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_date)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		uid, acct, ttype, amount, desc, date).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "amount": amount, "type": ttype},
		map[string]any{"kind": "transaction_created", "id": id, "title": fmt.Sprintf("%s %.2f", ttype, amount), "route": "/finance/transactions"}, nil
}
