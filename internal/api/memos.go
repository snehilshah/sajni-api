package api

import (
	"net/http"
	"strings"
)

func registerMemoRoutes(mux *http.ServeMux, deps Deps) {
	d := deps.DB
	mux.HandleFunc("GET /api/memos", listMemos(deps))
	mux.HandleFunc("POST /api/memos", createMemo(deps))
	mux.HandleFunc("PUT /api/memos/{id}", updateMemo(deps))
	mux.HandleFunc("DELETE /api/memos/{id}", deleteMemo(deps))
	_ = d
}

func listMemos(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())

		base := "SELECT m.id, m.content, m.pinned, m.created_at, m.updated_at FROM memos m"
		args := []any{uid}
		clauses := []string{"m.user_id = $1"}
		ph := 2

		if s := queryParam(r, "search"); s != "" {
			clauses = append(clauses, "m.content ILIKE $"+itoa(ph))
			args = append(args, "%"+s+"%")
			ph++
		}
		if queryParam(r, "pinned") == "true" {
			clauses = append(clauses, "m.pinned = TRUE")
		}
		if tag := queryParam(r, "tag"); tag != "" {
			base = "SELECT m.id, m.content, m.pinned, m.created_at, m.updated_at FROM memos m INNER JOIN tags t ON t.user_id = m.user_id AND t.entity_type = 'memo' AND t.entity_id = m.id"
			clauses = append(clauses, "t.tag = $"+itoa(ph))
			args = append(args, tag)
			ph++
		}

		q := base + " WHERE " + strings.Join(clauses, " AND ") + " ORDER BY m.pinned DESC, m.created_at DESC"
		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type Memo struct {
			ID        int64    `json:"id"`
			Content   string   `json:"content"`
			Pinned    bool     `json:"pinned"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"created_at"`
			UpdatedAt string   `json:"updated_at"`
		}
		var memos []Memo
		for rows.Next() {
			var m Memo
			if err := rows.Scan(&m.ID, &m.Content, &m.Pinned, &m.CreatedAt, &m.UpdatedAt); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			tagRows, err := d.Query("SELECT tag FROM tags WHERE user_id = $1 AND entity_type = 'memo' AND entity_id = $2", uid, m.ID)
			if err == nil {
				for tagRows.Next() {
					var t string
					tagRows.Scan(&t)
					m.Tags = append(m.Tags, t)
				}
				tagRows.Close()
			}
			if m.Tags == nil {
				m.Tags = []string{}
			}
			memos = append(memos, m)
		}
		if memos == nil {
			memos = []Memo{}
		}
		writeJSON(w, 200, memos)
	}
}

func createMemo(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Content string `json:"content"`
			Pinned  bool   `json:"pinned"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		var id int64
		err := d.QueryRow(
			"INSERT INTO memos (user_id, content, pinned) VALUES ($1, $2, $3) RETURNING id",
			uid, body.Content, body.Pinned,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		syncTags(d, uid, "memo", id, body.Content)
		syncBacklinks(d, uid, "memo", id, body.Content)
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateMemo(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Content *string `json:"content"`
			Pinned  *bool   `json:"pinned"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Content != nil {
			d.Exec("UPDATE memos SET content = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Content, id, uid)
			syncTags(d, uid, "memo", id, *body.Content)
			syncBacklinks(d, uid, "memo", id, *body.Content)
		}
		if body.Pinned != nil {
			d.Exec("UPDATE memos SET pinned = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *body.Pinned, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteMemo(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM tags WHERE user_id = $1 AND entity_type = 'memo' AND entity_id = $2", uid, id)
		d.Exec("DELETE FROM backlinks WHERE user_id = $1 AND source_type = 'memo' AND source_id = $2", uid, id)
		d.Exec("DELETE FROM memos WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// itoa is a small int-to-string for placeholder building; avoids strconv import bloat in many files.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
