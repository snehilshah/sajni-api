package ai

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"sajni/internal/db"
)

// Bookmark tools. The AI path skips the page-metadata fetch the HTTP
// handler does (no SSRF surface, no latency inside a tool round) — the
// favicon falls back to the icon service and the UI's brand-icon map.

var aiVideoHosts = map[string]bool{
	"youtube.com": true, "youtu.be": true, "vimeo.com": true,
	"twitch.tv": true, "dailymotion.com": true,
}

func aiBookmarkKind(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	if aiVideoHosts[host] {
		return "video"
	}
	return "site"
}

func listBookmarksTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	clauses := []string{"user_id = $1", "archived = FALSE"}
	vals := []any{uid}

	if k := argStr(args, "kind"); k == "video" || k == "site" {
		vals = append(vals, k)
		clauses = append(clauses, fmt.Sprintf("kind = $%d", len(vals)))
	}
	if v, ok := args["unread"]; ok {
		if b, ok := v.(bool); ok {
			clauses = append(clauses, fmt.Sprintf("unread = %t", b))
		}
	}
	if q := argStr(args, "query"); q != "" {
		vals = append(vals, "%"+q+"%")
		p := len(vals)
		clauses = append(clauses, fmt.Sprintf("(title ILIKE $%d OR url ILIKE $%d OR note ILIKE $%d)", p, p, p))
	}
	limit := argInt(args, "limit", 30)

	rows, err := d.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, url, kind, title, site_name, note, unread, created_at::date
		FROM bookmarks WHERE %s
		ORDER BY created_at DESC LIMIT %d`, strings.Join(clauses, " AND "), limit), vals...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type row struct {
		ID       int64  `json:"id"`
		URL      string `json:"url"`
		Kind     string `json:"kind"`
		Title    string `json:"title"`
		SiteName string `json:"site_name,omitempty"`
		Note     string `json:"note,omitempty"`
		Unread   bool   `json:"unread"`
		Added    string `json:"added"`
	}
	out := []row{}
	for rows.Next() {
		var b row
		if err := rows.Scan(&b.ID, &b.URL, &b.Kind, &b.Title, &b.SiteName, &b.Note, &b.Unread, &b.Added); err != nil {
			return nil, nil, err
		}
		out = append(out, b)
	}
	return out, nil, nil
}

func createBookmarkTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	raw := strings.TrimSpace(argStr(args, "url"))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, fmt.Errorf("invalid url")
	}
	title := strings.TrimSpace(argStr(args, "title"))
	if title == "" {
		title = u.Host
	}
	note := argStr(args, "note")
	favicon := "https://icons.duckduckgo.com/ip3/" + u.Hostname() + ".ico"

	var id int64
	err = d.QueryRowContext(ctx, `
		INSERT INTO bookmarks (user_id, url, kind, title, note, favicon_url)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		uid, u.String(), aiBookmarkKind(u), title, note, favicon).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "title": title},
		map[string]any{"kind": "bookmark_created", "id": id, "title": title, "route": "/media"}, nil
}

func updateBookmarkTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing id")
	}

	sets := []string{}
	vals := []any{}
	if v, ok := args["unread"]; ok {
		if b, ok := v.(bool); ok {
			vals = append(vals, b)
			sets = append(sets, fmt.Sprintf("unread = $%d", len(vals)))
		}
	}
	if v, ok := args["archived"]; ok {
		if b, ok := v.(bool); ok {
			vals = append(vals, b)
			sets = append(sets, fmt.Sprintf("archived = $%d", len(vals)))
		}
	}
	if t := strings.TrimSpace(argStr(args, "title")); t != "" {
		vals = append(vals, t)
		sets = append(sets, fmt.Sprintf("title = $%d", len(vals)))
	}
	if _, ok := args["note"]; ok {
		vals = append(vals, argStr(args, "note"))
		sets = append(sets, fmt.Sprintf("note = $%d", len(vals)))
	}
	if len(sets) == 0 {
		return nil, nil, fmt.Errorf("nothing to update: pass unread, archived, title or note")
	}

	vals = append(vals, id, uid)
	q := fmt.Sprintf("UPDATE bookmarks SET %s, updated_at = NOW() WHERE id = $%d AND user_id = $%d",
		strings.Join(sets, ", "), len(vals)-1, len(vals))
	res, err := d.ExecContext(ctx, q, vals...)
	if err != nil {
		return nil, nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil, fmt.Errorf("bookmark %d not found", id)
	}
	return map[string]any{"id": id, "updated": true},
		map[string]any{"kind": "bookmark_updated", "id": id, "route": "/media"}, nil
}
