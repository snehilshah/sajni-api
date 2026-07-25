package api

import (
	"net/http"
	"strings"
	"time"

	"sajni/internal/db"
)

// Slates answer one question: is this transaction part of normal life, or
// not? Every txn carries exactly one (fin_transactions.slate_id NOT NULL).
// `Plain` is normal life — system-owned, undeletable, the default for
// everything including cron-posted txns. Budgets ignore slates they do not
// explicitly name, and that exclusion is the whole feature. See SLATES.md.

func registerSlateRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/finance/slates", listSlates(deps))
	mux.HandleFunc("POST /api/finance/slates", createSlate(deps))
	mux.HandleFunc("PUT /api/finance/slates/{id}", updateSlate(deps))
	mux.HandleFunc("DELETE /api/finance/slates/{id}", deleteSlate(deps))
	// Lives here rather than with the other transaction routes: the sweep is a
	// slate operation that happens to name transactions, and it is the primary
	// way slates get filled.
	mux.HandleFunc("POST /api/finance/transactions/move-slate", moveTransactionsToSlate(deps))
}

// ensureSlates seeds a user's Plain slate and the pre-made One-offs slate.
// Mirrors ensureDefaultCategories: One-offs is an ordinary slate that merely
// ships ready-made — nothing downstream treats it specially. Only Plain is.
func ensureSlates(deps Deps, uid string) {
	d := deps.DB
	d.Exec(`INSERT INTO fin_slates (user_id, name, color, is_plain) VALUES ($1,'Plain','#6B7280',TRUE)
		ON CONFLICT (user_id) WHERE is_plain DO NOTHING`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, color)
		SELECT $1,'One-offs','#A14B4F'
		WHERE NOT EXISTS (SELECT 1 FROM fin_slates WHERE user_id = $1 AND NOT is_plain)`, uid)
}

// plainSlateID resolves the user's Plain slate, seeding it if a pre-slates
// account somehow reaches a write path before ensureSlates has run.
func plainSlateID(d *db.DB, uid string) (int64, error) {
	var id int64
	err := d.QueryRow(`SELECT id FROM fin_slates WHERE user_id = $1 AND is_plain`, uid).Scan(&id)
	if err == nil {
		return id, nil
	}
	if _, ierr := d.Exec(`INSERT INTO fin_slates (user_id, name, color, is_plain) VALUES ($1,'Plain','#6B7280',TRUE)
		ON CONFLICT (user_id) WHERE is_plain DO NOTHING`, uid); ierr != nil {
		return 0, ierr
	}
	return id, d.QueryRow(`SELECT id FROM fin_slates WHERE user_id = $1 AND is_plain`, uid).Scan(&id)
}

// resolveSlateID validates a requested slate, falling back to Plain. There is
// no active-slate mode and no null state: absent means Plain, always.
func resolveSlateID(d *db.DB, uid string, requested *int64) (int64, string) {
	plain, err := plainSlateID(d, uid)
	if err != nil {
		return 0, "no slate available"
	}
	if requested == nil || *requested == 0 {
		return plain, ""
	}
	var ok bool
	d.QueryRow(`SELECT EXISTS (SELECT 1 FROM fin_slates WHERE id = $1 AND user_id = $2 AND NOT archived)`,
		*requested, uid).Scan(&ok)
	if !ok {
		return 0, "slate not found"
	}
	return *requested, ""
}

type slateResp struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	// IsPlain marks normal life. Exactly one per user, never deletable.
	IsPlain  bool `json:"is_plain"`
	Archived bool `json:"archived"`
	// TxnCount is the LIFETIME count, not this month's — it drives the delete
	// warning, and a windowed count there would understate what moves to Plain.
	TxnCount   int64   `json:"txn_count"`
	TotalSpend float64 `json:"total_spend"`
	MonthSpend float64 `json:"month_spend"`
}

func listSlates(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		ensureSlates(deps, uid)
		includeArchived := queryParam(r, "include_archived") == "true"

		now := userNow(d, uid)
		from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		to := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Format("2006-01-02")

		q := `SELECT s.id, s.name, s.color, s.is_plain, s.archived,
			COUNT(t.id),
			COALESCE(SUM(t.amount) FILTER (WHERE t.type = 'expense'), 0),
			COALESCE(SUM(t.amount) FILTER (WHERE t.type = 'expense'
				AND (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3), 0)
		FROM fin_slates s
		LEFT JOIN fin_transactions t ON t.slate_id = s.id
		WHERE s.user_id = $1`
		if !includeArchived {
			q += ` AND NOT s.archived`
		}
		q += ` GROUP BY s.id ORDER BY s.is_plain DESC, s.created_at ASC`

		rows, err := d.Query(q, uid, from, to)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		items := []slateResp{}
		for rows.Next() {
			var s slateResp
			rows.Scan(&s.ID, &s.Name, &s.Color, &s.IsPlain, &s.Archived,
				&s.TxnCount, &s.TotalSpend, &s.MonthSpend)
			items = append(items, s)
		}
		writeJSON(w, 200, map[string]any{"items": items})
	}
}

func createSlate(deps Deps) http.HandlerFunc {
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
		d.QueryRow(`SELECT 1 FROM fin_slates WHERE user_id = $1 AND LOWER(name) = LOWER($2) AND NOT archived`,
			uid, b.Name).Scan(&dup)
		if dup == 1 {
			errJSON(w, 400, "a slate with that name already exists")
			return
		}
		if b.Color == "" {
			b.Color = "#2D5A4F"
		}
		var id int64
		if err := d.QueryRow(`INSERT INTO fin_slates (user_id, name, color) VALUES ($1,$2,$3) RETURNING id`,
			uid, b.Name, b.Color).Scan(&id); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateSlate(deps Deps) http.HandlerFunc {
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
		var isPlain bool
		if err := d.QueryRow(`SELECT is_plain FROM fin_slates WHERE id = $1 AND user_id = $2`, id, uid).Scan(&isPlain); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		// Plain is where everything lands by default; renaming or retiring it
		// would leave the default with no name, so it stays fixed.
		if isPlain {
			errJSON(w, 400, "Plain cannot be renamed or archived")
			return
		}
		if b.Name != nil {
			n := strings.TrimSpace(*b.Name)
			if n == "" {
				errJSON(w, 400, "name required")
				return
			}
			d.Exec(`UPDATE fin_slates SET name = $1 WHERE id = $2 AND user_id = $3`, n, id, uid)
		}
		if b.Color != nil {
			d.Exec(`UPDATE fin_slates SET color = $1 WHERE id = $2 AND user_id = $3`, *b.Color, id, uid)
		}
		if b.Archived != nil {
			d.Exec(`UPDATE fin_slates SET archived = $1 WHERE id = $2 AND user_id = $3`, *b.Archived, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// moveTransactionsToSlate is the retroactive sweep. Outliers are recognised
// after the money is spent, so bulk reassignment is the model's main verb, not
// a convenience over N single edits. slate_id 0 (or absent) means Plain, which
// is how something wrongly marked an outlier returns to normal life.
func moveTransactionsToSlate(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			TransactionIDs []int64 `json:"transaction_ids"`
			SlateID        int64   `json:"slate_id"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if len(b.TransactionIDs) == 0 {
			errJSON(w, 400, "transaction_ids required")
			return
		}

		target, bad := resolveSlateID(d, uid, &b.SlateID)
		if bad != "" {
			errJSON(w, 400, bad)
			return
		}
		var name string
		if err := d.QueryRow(`SELECT name FROM fin_slates WHERE id = $1 AND user_id = $2`, target, uid).Scan(&name); err != nil {
			internalError(w, r, "read slate name", err)
			return
		}

		// Both legs of a transfer share a slate, so a selected leg drags its
		// partner along — otherwise the pair splits across normal life and an
		// outlier and the ledger stops balancing.
		res, err := d.Exec(`UPDATE fin_transactions SET slate_id = $1, updated_at = NOW()
			WHERE user_id = $2 AND (id = ANY($3) OR transfer_pair = ANY($3))`,
			target, uid, b.TransactionIDs)
		if err != nil {
			internalError(w, r, "move transactions to slate", err)
			return
		}
		moved, _ := res.RowsAffected()
		writeJSON(w, 200, map[string]any{"moved": moved, "slate_id": target, "slate_name": name})
	}
}

// deleteSlate is free when the slate is empty. When it still holds
// transactions the caller must pass ?move_to_plain=true: they all move to
// Plain, which retroactively changes every baseline budget covering that
// period, so the client is expected to have warned with the real count first.
func deleteSlate(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		ctx := r.Context()
		var isPlain bool
		if err := d.QueryRowContext(ctx, `SELECT is_plain FROM fin_slates WHERE id = $1 AND user_id = $2`, id, uid).Scan(&isPlain); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		if isPlain {
			errJSON(w, 400, "Plain cannot be deleted")
			return
		}
		var n int64
		d.QueryRowContext(ctx, `SELECT COUNT(*) FROM fin_transactions WHERE slate_id = $1 AND user_id = $2`, id, uid).Scan(&n)
		if n > 0 && queryParam(r, "move_to_plain") != "true" {
			writeJSON(w, 409, map[string]any{
				"error":     "slate still holds transactions",
				"txn_count": n,
			})
			return
		}

		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin slate delete", err)
			return
		}
		defer tx.Rollback()
		if n > 0 {
			plain, perr := plainSlateID(d, uid)
			if perr != nil {
				internalError(w, r, "resolve plain slate", perr)
				return
			}
			// FK is RESTRICT, so this reassignment must land before the delete.
			if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET slate_id = $1 WHERE slate_id = $2 AND user_id = $3`,
				plain, id, uid); err != nil {
				internalError(w, r, "move transactions to plain", err)
				return
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_slates WHERE id = $1 AND user_id = $2`, id, uid); err != nil {
			internalError(w, r, "delete slate", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit slate delete", err)
			return
		}
		writeJSON(w, 200, map[string]any{"status": "ok", "moved": n})
	}
}
