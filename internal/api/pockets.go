package api

import (
	"net/http"
	"strings"

	"sajni/internal/db"
)

// Pockets are curated spend contexts ("Goa Trip"): every txn lives in
// exactly one (NULL = the implicit General pocket). The active pocket is
// the default for direct txn creation paths; see resolvePocketID. Budgets
// may filter their overall spend to a pocket set (fin_budget_pockets).

func registerPocketRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/finance/pockets", listPockets(deps))
	mux.HandleFunc("POST /api/finance/pockets", createPocket(deps))
	mux.HandleFunc("PUT /api/finance/pockets/{id}", updatePocket(deps))
	mux.HandleFunc("DELETE /api/finance/pockets/{id}", deletePocket(deps))
	mux.HandleFunc("POST /api/finance/pockets/active", setActivePocket(deps))
}

// activePocketID returns the user's active pocket id, nil when none.
func activePocketID(d *db.DB, uid string) *int64 {
	var id int64
	if err := d.QueryRow(`SELECT id FROM fin_pockets WHERE user_id = $1 AND is_active AND NOT archived`,
		uid).Scan(&id); err != nil {
		return nil
	}
	return &id
}

// listPockets powers the chip bar: pockets with their current-IST-month
// expense spend, plus the General (NULL-pocket) spend and the active id.
func listPockets(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		includeArchived := queryParam(r, "include_archived") == "true"

		now := userNow(d, uid)
		from, to := budgetWindow("monthly", "", "", "", now)

		type pocket struct {
			ID         int64   `json:"id"`
			Name       string  `json:"name"`
			Color      string  `json:"color"`
			IsActive   bool    `json:"is_active"`
			Archived   bool    `json:"archived"`
			MonthSpend float64 `json:"month_spend"`
			TxnCount   int64   `json:"txn_count"`
		}

		q := `SELECT id, name, color, is_active, archived FROM fin_pockets WHERE user_id = $1`
		if !includeArchived {
			q += " AND archived = FALSE"
		}
		q += " ORDER BY created_at ASC"
		rows, err := d.Query(q, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		items := []pocket{}
		var activeID *int64
		for rows.Next() {
			var p pocket
			rows.Scan(&p.ID, &p.Name, &p.Color, &p.IsActive, &p.Archived)
			if p.IsActive {
				id := p.ID
				activeID = &id
			}
			items = append(items, p)
		}
		rows.Close()

		// One grouped pass over the month's expenses; NULL group = General.
		spend := map[int64]struct {
			sum float64
			cnt int64
		}{}
		var generalSpend float64
		srows, serr := d.Query(`SELECT pocket_id, COALESCE(SUM(amount),0), COUNT(*)
			FROM fin_transactions
			WHERE user_id = $1 AND type = 'expense'
			AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3
			GROUP BY pocket_id`, uid, from, to)
		if serr == nil {
			for srows.Next() {
				var pid *int64
				var sum float64
				var cnt int64
				srows.Scan(&pid, &sum, &cnt)
				if pid == nil {
					generalSpend = sum
				} else {
					spend[*pid] = struct {
						sum float64
						cnt int64
					}{sum, cnt}
				}
			}
			srows.Close()
		}
		for i := range items {
			if s, ok := spend[items[i].ID]; ok {
				items[i].MonthSpend = s.sum
				items[i].TxnCount = s.cnt
			}
		}

		writeJSON(w, 200, map[string]any{
			"items":            items,
			"general_spend":    generalSpend,
			"active_pocket_id": activeID,
		})
	}
}

func createPocket(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		b.Name = strings.TrimSpace(b.Name)
		if b.Name == "" {
			errJSON(w, 400, "name required")
			return
		}
		var dup int
		d.QueryRow(`SELECT 1 FROM fin_pockets WHERE user_id = $1 AND LOWER(name) = LOWER($2) AND NOT archived`,
			uid, b.Name).Scan(&dup)
		if dup == 1 {
			errJSON(w, 400, "a pocket with that name already exists")
			return
		}
		if b.Color == "" {
			b.Color = "#2D5A4F"
		}
		var id int64
		if err := d.QueryRow(`INSERT INTO fin_pockets (user_id, name, color) VALUES ($1,$2,$3) RETURNING id`,
			uid, b.Name, b.Color).Scan(&id); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updatePocket(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			Name     *string `json:"name"`
			Color    *string `json:"color"`
			Archived *bool   `json:"archived"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Name != nil {
			n := strings.TrimSpace(*b.Name)
			if n == "" {
				errJSON(w, 400, "name required")
				return
			}
			d.Exec(`UPDATE fin_pockets SET name = $1 WHERE id = $2 AND user_id = $3`, n, id, uid)
		}
		if b.Color != nil {
			d.Exec(`UPDATE fin_pockets SET color = $1 WHERE id = $2 AND user_id = $3`, *b.Color, id, uid)
		}
		if b.Archived != nil {
			// Archiving the active pocket also deactivates it — an archived
			// pocket must never keep collecting new txns.
			d.Exec(`UPDATE fin_pockets SET archived = $1, is_active = (is_active AND NOT $1) WHERE id = $2 AND user_id = $3`,
				*b.Archived, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deletePocket(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		// FK SET NULL moves its txns to General; budget filter rows cascade.
		d.Exec(`DELETE FROM fin_pockets WHERE id = $1 AND user_id = $2`, id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// setActivePocket sets or clears the active pocket. pocket_id null/0 clears.
func setActivePocket(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			PocketID *int64 `json:"pocket_id"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		tx, err := d.Begin()
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`UPDATE fin_pockets SET is_active = FALSE WHERE user_id = $1 AND is_active`, uid); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		if b.PocketID != nil && *b.PocketID > 0 {
			res, err := tx.Exec(`UPDATE fin_pockets SET is_active = TRUE WHERE id = $1 AND user_id = $2 AND NOT archived`,
				*b.PocketID, uid)
			if err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			if n, _ := res.RowsAffected(); n == 0 {
				errJSON(w, 404, "pocket not found")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}
