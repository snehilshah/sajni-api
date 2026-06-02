package ai

import (
	"context"
	"database/sql"
	"strings"

	"sajni/internal/db"
)

func listInsightsTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	window := argStr(args, "window")
	limit := argInt(args, "limit", 10)
	q := `SELECT id, window_key, kind, title, body, score, generated_at::text
		FROM insights WHERE user_id = $1 AND dismissed_at IS NULL`
	vals := []any{uid}
	if window != "" {
		q += " AND window_key = $2"
		vals = append(vals, window)
	}
	q += " ORDER BY pinned DESC, generated_at DESC LIMIT $" + itoaInt(len(vals)+1)
	vals = append(vals, limit)

	rows, err := d.QueryContext(ctx, q, vals...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var w, kind, title, body, gen string
		var score float64
		rows.Scan(&id, &w, &kind, &title, &body, &score, &gen)
		out = append(out, map[string]any{
			"id": id, "window": w, "kind": kind, "title": title,
			"body": body, "score": score, "generated_at": gen,
		})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

// timeTravelTool runs a Postgres full-text + ILIKE union across journals,
// memos, notes, transactions, media titles, and journal location pills.
// Results are merged by descending date and trimmed. Cheap: every leg is
// indexed and capped to its own row budget.
func timeTravelTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	q := strings.TrimSpace(argStr(args, "query"))
	if q == "" {
		return nil, nil, errMissingQuery
	}
	limit := argInt(args, "limit", 10)
	if limit > 50 {
		limit = 50
	}
	typesAllowed := map[string]bool{}
	for _, t := range argStrSlice(args, "types") {
		typesAllowed[t] = true
	}
	allowAll := len(typesAllowed) == 0
	from := argStr(args, "date_from")
	to := argStr(args, "date_to")

	type Hit struct {
		Type    string `json:"type"`
		ID      int64  `json:"id"`
		Date    string `json:"date"`
		Title   string `json:"title"`
		Excerpt string `json:"excerpt"`
	}
	var hits []Hit
	like := "%" + q + "%"

	dateClause := ""
	dateArgs := []any{}
	idx := 3 // $1=uid, $2=like
	if from != "" {
		dateClause += " AND %s >= $" + itoaInt(idx)
		dateArgs = append(dateArgs, from)
		idx++
	}
	if to != "" {
		dateClause += " AND %s <= $" + itoaInt(idx)
		dateArgs = append(dateArgs, to)
		idx++
	}
	expand := func(col string) string {
		return strings.ReplaceAll(dateClause, "%s", col)
	}

	if allowAll || typesAllowed["journal"] {
		query := `SELECT id, date::text, COALESCE(location_label,''), COALESCE(mood,'') FROM journal_entries
			WHERE user_id=$1 AND (location_label ILIKE $2 OR mood ILIKE $2)` + expand("date") +
			` ORDER BY date DESC LIMIT 20`
		args := append([]any{uid, like}, dateArgs...)
		rows, err := d.QueryContext(ctx, query, args...)
		if err == nil {
			for rows.Next() {
				var id int64
				var date, loc, mood string
				rows.Scan(&id, &date, &loc, &mood)
				excerpt := loc
				if excerpt == "" {
					excerpt = "mood: " + mood
				}
				hits = append(hits, Hit{Type: "journal", ID: id, Date: date, Title: date, Excerpt: excerpt})
			}
			rows.Close()
		}
	}
	if allowAll || typesAllowed["memo"] {
		query := `SELECT id, created_at::date::text, content FROM memos
			WHERE user_id=$1 AND content ILIKE $2` + expand("created_at::date") +
			` ORDER BY created_at DESC LIMIT 20`
		args := append([]any{uid, like}, dateArgs...)
		rows, err := d.QueryContext(ctx, query, args...)
		if err == nil {
			for rows.Next() {
				var id int64
				var date, content string
				rows.Scan(&id, &date, &content)
				hits = append(hits, Hit{Type: "memo", ID: id, Date: date, Title: truncForHit(content, 60), Excerpt: truncForHit(content, 240)})
			}
			rows.Close()
		}
	}
	if allowAll || typesAllowed["note"] {
		query := `SELECT id, updated_at::date::text, title FROM notes
			WHERE user_id=$1 AND title ILIKE $2` + expand("updated_at::date") +
			` ORDER BY updated_at DESC LIMIT 20`
		args := append([]any{uid, like}, dateArgs...)
		rows, err := d.QueryContext(ctx, query, args...)
		if err == nil {
			for rows.Next() {
				var id int64
				var date, title string
				rows.Scan(&id, &date, &title)
				hits = append(hits, Hit{Type: "note", ID: id, Date: date, Title: title, Excerpt: title})
			}
			rows.Close()
		}
	}
	if allowAll || typesAllowed["transaction"] {
		query := `SELECT t.id, (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date::text, t.description, t.amount, a.name FROM fin_transactions t
			JOIN fin_accounts a ON a.id = t.account_id
			WHERE t.user_id=$1 AND t.description ILIKE $2` + expand("(t.txn_at AT TIME ZONE 'Asia/Kolkata')::date") +
			` ORDER BY t.txn_at DESC LIMIT 20`
		args := append([]any{uid, like}, dateArgs...)
		rows, err := d.QueryContext(ctx, query, args...)
		if err == nil {
			for rows.Next() {
				var id int64
				var date, desc, account string
				var amount float64
				rows.Scan(&id, &date, &desc, &amount, &account)
				hits = append(hits, Hit{Type: "transaction", ID: id, Date: date, Title: desc, Excerpt: account})
			}
			rows.Close()
		}
	}
	if allowAll || typesAllowed["media"] {
		query := `SELECT id, COALESCE(updated_at::date::text, created_at::date::text), title, type FROM media
			WHERE user_id=$1 AND title ILIKE $2` + expand("COALESCE(updated_at, created_at)::date") +
			` ORDER BY updated_at DESC NULLS LAST, created_at DESC LIMIT 20`
		args := append([]any{uid, like}, dateArgs...)
		rows, err := d.QueryContext(ctx, query, args...)
		if err == nil {
			for rows.Next() {
				var id int64
				var date sql.NullString
				var title, mtype string
				rows.Scan(&id, &date, &title, &mtype)
				dStr := ""
				if date.Valid {
					dStr = date.String
				}
				hits = append(hits, Hit{Type: "media", ID: id, Date: dStr, Title: title, Excerpt: mtype})
			}
			rows.Close()
		}
	}

	// Sort by date desc (lexicographic on ISO date works fine).
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].Date > hits[j-1].Date; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if int64(len(hits)) > limit {
		hits = hits[:limit]
	}
	return map[string]any{"items": hits, "count": len(hits), "query": q}, nil, nil
}

func truncForHit(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

type errStr string

func (e errStr) Error() string { return string(e) }

const errMissingQuery = errStr("missing query")
