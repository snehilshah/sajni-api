package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/genai"

	"sajni/internal/db"
	"sajni/internal/storage"
)

// fetchMaxChars caps a note or journal body handed back to the model.
const fetchMaxChars = 20000

// fetchTables maps a ref's type to its table. Each row comes back whole
// (minus user_id) so the model sees every field the app does.
var fetchTables = map[string]string{
	"task":        "tasks",
	"memo":        "memos",
	"note":        "notes",
	"transaction": "fin_transactions",
	"media":       "media",
	"bookmark":    "bookmarks",
	"reminder":    "reminders",
	"habit":       "habits",
	"project":     "thinking_projects",
	"event":       "events",
}

func fetchTool(d *db.DB, store storage.Storage) Tool {
	return Tool{
		Name: "fetch",
		Description: "Read one item in full by its ref, as search and the list tools give them: " +
			"task:12, note:5 (with its text), memo:3, transaction:88, media:4, bookmark:9, reminder:2, habit:1, event:7, project:6, " +
			"or journal:2026-11-16. Use it when a list or search shows the item but not the detail you need. " +
			"Timestamps are UTC; convert to the user's time zone before reading out a day or time.",
		Schema: obj(map[string]*genai.Schema{
			"ref": str("type:id, e.g. note:5 or journal:2026-11-16."),
		}, "ref"),
		Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
			return runFetch(ctx, d, store, uid, argStr(args, "ref"))
		},
	}
}

func runFetch(ctx context.Context, d *db.DB, store storage.Storage, uid, ref string) (any, map[string]any, error) {
	kind, key, ok := strings.Cut(strings.TrimSpace(ref), ":")
	kind = strings.ToLower(strings.TrimSpace(kind))
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return nil, nil, fmt.Errorf("ref must look like note:5 or journal:2026-11-16")
	}
	if kind == "journal" {
		out, meta, err := getJournalTool(ctx, d, store, uid, key)
		if m, ok := out.(map[string]any); ok {
			if text, ok := m["content"].(string); ok {
				if clipped, cut := clipChars(text, fetchMaxChars); cut {
					m["content"], m["truncated"] = clipped, true
				}
			}
		}
		return out, meta, err
	}
	table, known := fetchTables[kind]
	if !known {
		return nil, nil, fmt.Errorf("unknown ref type %q", kind)
	}
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("%s ids are numbers", kind)
	}
	var raw []byte
	err = d.QueryRowContext(ctx, `SELECT jsonb_strip_nulls(to_jsonb(x) - 'user_id') FROM `+table+` x WHERE id=$1 AND user_id=$2`, id, uid).Scan(&raw)
	if err != nil {
		return nil, nil, fmt.Errorf("no %s %d", kind, id)
	}
	row := map[string]any{}
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, nil, err
	}
	row["ref"] = kind + ":" + key
	switch kind {
	case "note":
		blobKey, _ := row["blob_key"].(string)
		delete(row, "blob_key")
		if blobKey != "" && store != nil {
			body, _, err := store.Get(ctx, blobKey)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return nil, nil, err
			}
			text, cut := clipChars(string(body), fetchMaxChars)
			row["content"] = text
			if cut {
				row["truncated"] = true
			}
		}
	case "task":
		var subs []byte
		d.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('ref','task:'||id,'title',title,'status',status) ORDER BY id),'[]')
			FROM tasks WHERE parent_task_id=$1 AND user_id=$2`, id, uid).Scan(&subs)
		var list any
		json.Unmarshal(subs, &list)
		row["subtasks"] = list
	case "transaction":
		var account, category string
		d.QueryRowContext(ctx, `SELECT COALESCE(a.name,''), COALESCE(c.name,'') FROM fin_transactions t
			LEFT JOIN fin_accounts a ON a.id=t.account_id LEFT JOIN fin_categories c ON c.id=t.category_id
			WHERE t.id=$1 AND t.user_id=$2`, id, uid).Scan(&account, &category)
		row["account"], row["category"] = account, category
	}
	return row, nil, nil
}

func clipChars(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	r := []rune(s)
	return string(r[:n]), true
}
