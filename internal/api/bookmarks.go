package api

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Bookmarks: saved links (read-later + keep-forever). Create fetches page
// metadata server-side through the same SSRF guard as the link preview
// (linkpreview.go); a failed fetch still saves the bookmark with blanks.

func registerBookmarkRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/bookmarks", listBookmarks(deps))
	mux.HandleFunc("POST /api/bookmarks", createBookmark(deps))
	mux.HandleFunc("PUT /api/bookmarks/{id}", updateBookmark(deps))
	mux.HandleFunc("DELETE /api/bookmarks/{id}", deleteBookmark(deps))
}

type bookmarkRow struct {
	ID         int64  `json:"id"`
	URL        string `json:"url"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	SiteName   string `json:"site_name"`
	FaviconURL string `json:"favicon_url"`
	ImageURL   string `json:"image_url"`
	Note       string `json:"note"`
	Unread     bool   `json:"unread"`
	Archived   bool   `json:"archived"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

const bookmarkCols = "id, url, kind, title, site_name, favicon_url, image_url, note, unread, archived, created_at, updated_at"

func scanBookmark(s interface{ Scan(...any) error }) (bookmarkRow, error) {
	var b bookmarkRow
	err := s.Scan(&b.ID, &b.URL, &b.Kind, &b.Title, &b.SiteName, &b.FaviconURL, &b.ImageURL, &b.Note, &b.Unread, &b.Archived, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

func listBookmarks(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())

		clauses := []string{"user_id = $1"}
		args := []any{uid}
		ph := 2

		if k := queryParam(r, "kind"); k == "video" || k == "site" {
			clauses = append(clauses, "kind = $"+itoa(ph))
			args = append(args, k)
			ph++
		}
		switch queryParam(r, "unread") {
		case "true":
			clauses = append(clauses, "unread = TRUE")
		case "false":
			clauses = append(clauses, "unread = FALSE")
		}
		// Archived rows are hidden unless explicitly requested.
		if queryParam(r, "archived") == "true" {
			clauses = append(clauses, "archived = TRUE")
		} else {
			clauses = append(clauses, "archived = FALSE")
		}
		if s := queryParam(r, "search"); s != "" {
			clauses = append(clauses, "(title ILIKE $"+itoa(ph)+" OR url ILIKE $"+itoa(ph)+" OR note ILIKE $"+itoa(ph)+" OR site_name ILIKE $"+itoa(ph)+")")
			args = append(args, "%"+s+"%")
			ph++
		}

		q := "SELECT " + bookmarkCols + " FROM bookmarks WHERE " + strings.Join(clauses, " AND ") + " ORDER BY created_at DESC"
		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		out := []bookmarkRow{}
		for rows.Next() {
			b, err := scanBookmark(rows)
			if err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			out = append(out, b)
		}
		writeJSON(w, 200, out)
	}
}

func createBookmark(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			URL   string `json:"url"`
			Title string `json:"title"`
			Note  string `json:"note"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		u, err := url.Parse(strings.TrimSpace(body.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errJSON(w, 400, "invalid url")
			return
		}

		meta := fetchBookmarkMeta(r.Context(), u)
		title := strings.TrimSpace(body.Title)
		if title == "" {
			title = meta.title
		}
		if title == "" {
			title = u.Host
		}

		var b bookmarkRow
		row := d.QueryRow(
			`INSERT INTO bookmarks (user_id, url, kind, title, site_name, favicon_url, image_url, note)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+bookmarkCols,
			uid, u.String(), bookmarkKind(u), title, meta.siteName, meta.favicon, meta.image, strings.TrimSpace(body.Note),
		)
		b, err = scanBookmark(row)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		syncTags(d, uid, "bookmark", b.ID, b.Note)
		writeJSON(w, 201, b)
	}
}

func updateBookmark(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Title    *string `json:"title"`
			Note     *string `json:"note"`
			Unread   *bool   `json:"unread"`
			Archived *bool   `json:"archived"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Title != nil {
			d.Exec("UPDATE bookmarks SET title = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", strings.TrimSpace(*body.Title), id, uid)
		}
		if body.Note != nil {
			d.Exec("UPDATE bookmarks SET note = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", strings.TrimSpace(*body.Note), id, uid)
			syncTags(d, uid, "bookmark", id, *body.Note)
		}
		if body.Unread != nil {
			d.Exec("UPDATE bookmarks SET unread = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Unread, id, uid)
		}
		if body.Archived != nil {
			d.Exec("UPDATE bookmarks SET archived = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Archived, id, uid)
		}
		row := d.QueryRow("SELECT "+bookmarkCols+" FROM bookmarks WHERE id = $1 AND user_id = $2", id, uid)
		b, err := scanBookmark(row)
		if err != nil {
			errJSON(w, 404, "not found")
			return
		}
		writeJSON(w, 200, b)
	}
}

func deleteBookmark(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM tags WHERE user_id = $1 AND entity_type = 'bookmark' AND entity_id = $2", uid, id)
		d.Exec("DELETE FROM bookmarks WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// ─── URL classification + metadata ──────────────────────────────────────

var videoHosts = map[string]bool{
	"youtube.com": true, "youtu.be": true, "vimeo.com": true,
	"twitch.tv": true, "dailymotion.com": true,
}

// bookmarkKind: 'video' for known video hosts, else 'site'.
func bookmarkKind(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	if videoHosts[host] {
		return "video"
	}
	return "site"
}

var (
	ogSiteRe  = regexp.MustCompile(`(?is)<meta[^>]+property\s*=\s*["']og:site_name["'][^>]*>`)
	ogImageRe = regexp.MustCompile(`(?is)<meta[^>]+property\s*=\s*["']og:image["'][^>]*>`)
	iconRe    = regexp.MustCompile(`(?is)<link[^>]+rel\s*=\s*["'](?:shortcut )?icon["'][^>]*>`)
	hrefRe    = regexp.MustCompile(`(?is)href\s*=\s*["']([^"']*)["']`)
)

type bookmarkMeta struct {
	title    string
	siteName string
	image    string
	favicon  string
}

// fetchBookmarkMeta pulls title/site/og-image/favicon. Best-effort: any
// failure returns whatever was gathered (possibly all blanks) so the save
// itself never fails on a slow or hostile page.
func fetchBookmarkMeta(ctx context.Context, u *url.URL) bookmarkMeta {
	meta := bookmarkMeta{
		// DuckDuckGo icon service as a guaranteed fallback; replaced below
		// if the page declares its own icon.
		favicon: "https://icons.duckduckgo.com/ip3/" + u.Hostname() + ".ico",
	}
	if err := guardURL(u); err != nil {
		return meta
	}

	fctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(fctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", "SajniBot/1.0 (+https://ohmysajni.com)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := previewClient.Do(req)
	if err != nil {
		return meta
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return meta
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	body := string(raw)

	meta.title = extractTitle(body)
	if tag := ogSiteRe.FindString(body); tag != "" {
		if m := contentRe.FindStringSubmatch(tag); len(m) == 2 {
			meta.siteName = clean(m[1])
		}
	}
	if tag := ogImageRe.FindString(body); tag != "" {
		if m := contentRe.FindStringSubmatch(tag); len(m) == 2 {
			meta.image = resolveRef(u, m[1])
		}
	}
	if tag := iconRe.FindString(body); tag != "" {
		if m := hrefRe.FindStringSubmatch(tag); len(m) == 2 {
			if icon := resolveRef(u, m[1]); icon != "" {
				meta.favicon = icon
			}
		}
	}
	return meta
}

// resolveRef makes a possibly-relative href absolute against the page URL.
// Returns "" for anything that doesn't resolve to http(s).
func resolveRef(base *url.URL, ref string) string {
	ref = strings.TrimSpace(html.UnescapeString(ref))
	if ref == "" {
		return ""
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	abs := base.ResolveReference(r)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	return abs.String()
}
