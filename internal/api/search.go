package api

import (
	"database/sql"
	"net/http"
	"strings"
)

func registerSearchRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/search", search(deps))
}

// SearchHit is the shape every entity type collapses into for the
// universal palette.
type SearchHit struct {
	Type     string `json:"type"`
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Snippet  string `json:"snippet,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	// Route is a frontend-routable URL hint. Optional — the client knows how
	// to navigate by type, but a server hint helps for deep links (e.g. notes).
	Route string `json:"route,omitempty"`
}

// search performs an ILIKE substring search across every searchable entity
// the user owns. Per-type limits keep the response small; the client does
// fuzzy ranking and smart-prefix filtering on top.
func search(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		q := strings.TrimSpace(queryParam(r, "q"))
		typeFilter := strings.TrimSpace(queryParam(r, "type"))

		results := make([]SearchHit, 0, 64)
		like := "%" + q + "%"

		want := func(t string) bool { return typeFilter == "" || typeFilter == t }

		// Memos
		if want("memo") {
			results = appendQuery(results, d, "memo",
				`SELECT id, content FROM memos
				 WHERE user_id = $1 AND ($2 = '' OR content ILIKE $3)
				 ORDER BY pinned DESC, updated_at DESC LIMIT 30`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var content string
					if err := rs.Scan(&id, &content); err != nil {
						return SearchHit{}, false
					}
					title := truncate(content, 80)
					if title == "" {
						return SearchHit{}, false
					}
					return SearchHit{Type: "memo", ID: id, Title: title, Route: "/memos"}, true
				})
		}

		// Tasks
		if want("task") {
			results = appendQuery(results, d, "task",
				`SELECT id, title, status, priority FROM tasks
				 WHERE user_id = $1 AND ($2 = '' OR title ILIKE $3 OR description ILIKE $3)
				 ORDER BY (status='done') ASC, updated_at DESC LIMIT 30`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var title, status, priority string
					if err := rs.Scan(&id, &title, &status, &priority); err != nil {
						return SearchHit{}, false
					}
					if title == "" {
						return SearchHit{}, false
					}
					return SearchHit{Type: "task", ID: id, Title: title, Subtitle: priority + " · " + status, Route: "/tasks"}, true
				})
		}

		// Notes
		if want("note") {
			results = appendQuery(results, d, "note",
				`SELECT id, title, folder FROM notes
				 WHERE user_id = $1 AND ($2 = '' OR title ILIKE $3 OR folder ILIKE $3)
				 ORDER BY updated_at DESC LIMIT 30`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var title, folder string
					if err := rs.Scan(&id, &title, &folder); err != nil {
						return SearchHit{}, false
					}
					if title == "" {
						title = "Untitled"
					}
					sub := folder
					return SearchHit{Type: "note", ID: id, Title: title, Subtitle: sub, Route: "/notes?id=" + itoa64(id)}, true
				})
		}

		// Journal entries (search by date string + mood)
		if want("journal") {
			results = appendQuery(results, d, "journal",
				`SELECT id, date::text, COALESCE(mood, '') FROM journal_entries
				 WHERE user_id = $1 AND ($2 = '' OR date::text ILIKE $3 OR mood ILIKE $3)
				 ORDER BY date DESC LIMIT 30`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var date, mood string
					if err := rs.Scan(&id, &date, &mood); err != nil {
						return SearchHit{}, false
					}
					return SearchHit{Type: "journal", ID: id, Title: date, Subtitle: mood, Route: "/journal?date=" + date}, true
				})
		}

		// Habits
		if want("habit") {
			results = appendQuery(results, d, "habit",
				`SELECT id, name, frequency FROM habits
				 WHERE user_id = $1 AND ($2 = '' OR name ILIKE $3)
				 ORDER BY id DESC LIMIT 30`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var name, freq string
					if err := rs.Scan(&id, &name, &freq); err != nil {
						return SearchHit{}, false
					}
					return SearchHit{Type: "habit", ID: id, Title: name, Subtitle: freq, Route: "/habits"}, true
				})
		}

		// Media
		if want("media") {
			results = appendQuery(results, d, "media",
				`SELECT id, title, type, status FROM media
				 WHERE user_id = $1 AND ($2 = '' OR title ILIKE $3 OR genre ILIKE $3 OR platform ILIKE $3)
				 ORDER BY updated_at DESC LIMIT 30`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var title, mtype, status string
					if err := rs.Scan(&id, &title, &mtype, &status); err != nil {
						return SearchHit{}, false
					}
					if title == "" {
						return SearchHit{}, false
					}
					return SearchHit{Type: "media", ID: id, Title: title, Subtitle: mtype + " · " + status, Route: "/media"}, true
				})
		}

		// Tags
		if want("tag") {
			results = appendQuery(results, d, "tag",
				`SELECT MIN(id), tag, COUNT(*) FROM tags
				 WHERE user_id = $1 AND ($2 = '' OR tag ILIKE $3)
				 GROUP BY tag ORDER BY COUNT(*) DESC, tag ASC LIMIT 30`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var tag string
					var count int
					if err := rs.Scan(&id, &tag, &count); err != nil {
						return SearchHit{}, false
					}
					return SearchHit{Type: "tag", ID: id, Title: "#" + tag, Subtitle: itoa(count) + " tagged", Route: "/tags/" + tag}, true
				})
		}

		// Finance accounts
		if want("account") {
			results = appendQuery(results, d, "account",
				`SELECT id, name, type, institution FROM fin_accounts
				 WHERE user_id = $1 AND archived = FALSE AND ($2 = '' OR name ILIKE $3 OR institution ILIKE $3)
				 ORDER BY updated_at DESC LIMIT 20`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var name, atype, inst string
					if err := rs.Scan(&id, &name, &atype, &inst); err != nil {
						return SearchHit{}, false
					}
					sub := atype
					if inst != "" {
						sub = atype + " · " + inst
					}
					return SearchHit{Type: "account", ID: id, Title: name, Subtitle: sub, Route: "/finance/accounts"}, true
				})
		}

		// Finance transactions
		if want("transaction") {
			results = appendQuery(results, d, "transaction",
				`SELECT t.id, t.description, t.amount::text, t.type, t.txn_date::text, COALESCE(a.name, '')
				 FROM fin_transactions t
				 LEFT JOIN fin_accounts a ON a.id = t.account_id
				 WHERE t.user_id = $1 AND ($2 = '' OR t.description ILIKE $3)
				 ORDER BY t.txn_date DESC LIMIT 30`,
				uid, q, like,
				func(rs *sql.Rows) (SearchHit, bool) {
					var id int64
					var desc, amount, ttype, date, account string
					if err := rs.Scan(&id, &desc, &amount, &ttype, &date, &account); err != nil {
						return SearchHit{}, false
					}
					title := desc
					if title == "" {
						title = ttype + " · " + amount
					}
					return SearchHit{Type: "transaction", ID: id, Title: title, Subtitle: account + " · " + date, Route: "/finance/transactions"}, true
				})
		}

		writeJSON(w, 200, map[string]any{"results": results})
	}
}

// appendQuery runs a query against d and applies a row-mapper, appending the
// successful results to dst. Any per-row error is silently skipped so a
// single malformed row doesn't blow up the whole palette.
func appendQuery(
	dst []SearchHit,
	d interface {
		Query(string, ...any) (*sql.Rows, error)
	},
	_ string,
	sqlText string,
	uid int64, q string, like string,
	mapper func(*sql.Rows) (SearchHit, bool),
) []SearchHit {
	rows, err := d.Query(sqlText, uid, q, like)
	if err != nil {
		return dst
	}
	defer rows.Close()
	for rows.Next() {
		if r, ok := mapper(rows); ok {
			dst = append(dst, r)
		}
	}
	return dst
}
