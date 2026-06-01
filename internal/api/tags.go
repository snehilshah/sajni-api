package api

import (
	"fmt"
	"net/http"
)

func registerTagRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/tags", listTags(deps))
	mux.HandleFunc("GET /api/tags/{tag}", getTagEntities(deps))
}

func listTags(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query("SELECT tag, COUNT(*) as cnt FROM tags WHERE user_id = $1 GROUP BY tag ORDER BY cnt DESC, tag ASC", uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type TagInfo struct {
			Tag   string `json:"tag"`
			Count int    `json:"count"`
		}
		var tags []TagInfo
		for rows.Next() {
			var t TagInfo
			rows.Scan(&t.Tag, &t.Count)
			tags = append(tags, t)
		}
		if tags == nil {
			tags = []TagInfo{}
		}
		writeJSON(w, 200, tags)
	}
}

func getTagEntities(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		tag := pathParam(r, "tag")
		rows, err := d.Query(
			"SELECT entity_type, entity_id FROM tags WHERE user_id = $1 AND tag = $2 ORDER BY entity_type, entity_id DESC",
			uid, tag,
		)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		type Entity struct {
			Type     string `json:"type"`
			ID       int64  `json:"id"`
			Title    string `json:"title"`
			Subtitle string `json:"subtitle,omitempty"`
		}
		var entities []Entity
		for rows.Next() {
			var e Entity
			rows.Scan(&e.Type, &e.ID)
			switch e.Type {
			case "memo":
				var c string
				d.QueryRow("SELECT content FROM memos WHERE user_id = $1 AND id = $2", uid, e.ID).Scan(&c)
				e.Title = truncate(c, 120)
			case "note":
				d.QueryRow("SELECT title FROM notes WHERE user_id = $1 AND id = $2", uid, e.ID).Scan(&e.Title)
			case "journal":
				d.QueryRow("SELECT date::text FROM journal_entries WHERE user_id = $1 AND id = $2", uid, e.ID).Scan(&e.Title)
			case "task":
				var status string
				d.QueryRow("SELECT title, status FROM tasks WHERE user_id = $1 AND id = $2", uid, e.ID).Scan(&e.Title, &status)
				e.Subtitle = status
			case "transaction":
				var desc string
				var amount float64
				var ttype string
				d.QueryRow("SELECT description, amount, type FROM fin_transactions WHERE user_id = $1 AND id = $2", uid, e.ID).Scan(&desc, &amount, &ttype)
				if desc != "" {
					e.Title = desc
				} else {
					e.Title = ttype
				}
				e.Subtitle = fmt.Sprintf("₹%.0f · %s", amount, ttype)
			}
			if e.Title == "" {
				continue
			}
			entities = append(entities, e)
		}
		if entities == nil {
			entities = []Entity{}
		}
		writeJSON(w, 200, map[string]any{"tag": tag, "entities": entities})
	}
}
