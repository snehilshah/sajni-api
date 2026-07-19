package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"sajni/internal/db"
)

// Shared pockets: spliit-style expense splitting. A shared pocket has
// participants (fin_pocket_members) — sajni users or text-only names —
// split expenses, settlements and an activity log. Members are full
// collaborators; only the owner manages the pocket itself, members and
// invites. Privacy contract: responses never carry other users' emails
// (owner excepted), user ids, or any personal-ledger data. A member's
// personal-ledger presence is an "echo" fin_transactions row linked via
// shared_expense_id/settlement_id, always pocket_id NULL, created only
// with an account the acting user picked themselves.

func registerSharedPocketRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /api/finance/pockets/{id}/share", sharePocket(deps))
	mux.HandleFunc("GET /api/finance/pockets/{id}", getPocketDetail(deps))
	mux.HandleFunc("POST /api/finance/pockets/{id}/members", addPocketMember(deps))
	mux.HandleFunc("PUT /api/finance/pockets/{id}/members/{mid}", renamePocketMember(deps))
	mux.HandleFunc("DELETE /api/finance/pockets/{id}/members/{mid}", removePocketMember(deps))
	mux.HandleFunc("POST /api/finance/pockets/{id}/leave", leavePocket(deps))
	mux.HandleFunc("GET /api/finance/pockets/{id}/expenses", listPocketExpenses(deps))
	mux.HandleFunc("POST /api/finance/pockets/{id}/expenses", createPocketExpense(deps))
	mux.HandleFunc("PUT /api/finance/pockets/{id}/expenses/{eid}", updatePocketExpense(deps))
	mux.HandleFunc("DELETE /api/finance/pockets/{id}/expenses/{eid}", deletePocketExpense(deps))
	mux.HandleFunc("POST /api/finance/pockets/{id}/expenses/{eid}/echo", attachExpenseEcho(deps))
	mux.HandleFunc("GET /api/finance/pockets/{id}/balances", pocketBalances(deps))
	mux.HandleFunc("GET /api/finance/pockets/{id}/settlements", listPocketSettlements(deps))
	mux.HandleFunc("POST /api/finance/pockets/{id}/settlements", createPocketSettlement(deps))
	mux.HandleFunc("DELETE /api/finance/pockets/{id}/settlements/{sid}", deletePocketSettlement(deps))
	mux.HandleFunc("POST /api/finance/pockets/{id}/settlements/{sid}/echo", attachSettlementEcho(deps))
	mux.HandleFunc("GET /api/finance/pockets/{id}/activity", pocketActivity(deps))
}

// --- access ------------------------------------------------------------------

type pocketAccess struct {
	OwnerID    string
	Kind       string
	Name       string
	IsOwner    bool
	MemberID   int64  // acting user's active member row; 0 for a personal pocket
	MemberName string // acting user's display name in this pocket
}

// requirePocketAccess resolves the caller's relationship to a pocket: owner
// or active member. Anyone else gets errFinanceReferenceNotFound → 404, so
// pocket existence never leaks.
func requirePocketAccess(ctx context.Context, q dbtx, uid string, pocketID int64) (*pocketAccess, error) {
	if pocketID == 0 {
		return nil, errFinanceReferenceNotFound
	}
	a := &pocketAccess{}
	var memberID sql.NullInt64
	var memberName sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT p.user_id, p.kind, p.name, p.user_id = $2, m.id, m.display_name
		FROM fin_pockets p
		LEFT JOIN fin_pocket_members m ON m.pocket_id = p.id AND m.user_id = $2 AND m.left_at IS NULL
		WHERE p.id = $1 AND (p.user_id = $2 OR m.id IS NOT NULL)`,
		pocketID, uid).Scan(&a.OwnerID, &a.Kind, &a.Name, &a.IsOwner, &memberID, &memberName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errFinanceReferenceNotFound
	}
	if err != nil {
		return nil, err
	}
	a.MemberID = memberID.Int64
	a.MemberName = memberName.String
	return a, nil
}

// requireSharedPocketAccess is requirePocketAccess plus a kind gate: split
// endpoints only make sense on shared pockets.
func requireSharedPocketAccess(ctx context.Context, q dbtx, uid string, pocketID int64) (*pocketAccess, error) {
	a, err := requirePocketAccess(ctx, q, uid, pocketID)
	if err != nil {
		return nil, err
	}
	if a.Kind != "shared" {
		return nil, errFinanceReferenceNotFound
	}
	return a, nil
}

func pocketAccessError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, errFinanceReferenceNotFound) {
		errJSON(w, 404, "not found")
		return
	}
	internalError(w, r, op, err)
}

// userDisplayName is the fallback member name for a sajni user: profile name,
// else the email local part.
func userDisplayName(ctx context.Context, q dbtx, uid string) string {
	var name string
	q.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(name,''), split_part(email::text,'@',1)) FROM users WHERE id = $1`, uid).Scan(&name)
	return name
}

func logPocketActivity(ctx context.Context, q dbtx, ownerID string, pocketID int64, actorName, kind string, detail map[string]any) {
	raw, _ := json.Marshal(detail)
	if detail == nil {
		raw = []byte("{}")
	}
	q.ExecContext(ctx, `INSERT INTO fin_pocket_activity (owner_id, pocket_id, actor_name, kind, detail) VALUES ($1,$2,$3,$4,$5)`,
		ownerID, pocketID, actorName, kind, raw)
}

func paise(amount float64) int64 { return int64(math.Round(amount * 100)) }

func rupees(p int64) float64 { return float64(p) / 100 }

// --- conversion ----------------------------------------------------------------

// sharePocket converts a personal pocket to shared (one-way). Existing
// expense txns become zero-debt shared expenses (paid by the owner, 100%
// owner share) and the original txn becomes the echo row, so the owner's
// ledger is untouched. Non-expense txns move to General.
func sharePocket(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			DisplayName string `json:"display_name"`
		}
		readJSON(r, &b)
		ctx := r.Context()
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin pocket share", err)
			return
		}
		defer tx.Rollback()

		res, err := tx.ExecContext(ctx, `UPDATE fin_pockets SET kind = 'shared', is_active = FALSE
			WHERE id = $1 AND user_id = $2 AND kind = 'personal' AND NOT archived`, id, uid)
		if err != nil {
			internalError(w, r, "share pocket", err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			errJSON(w, 404, "not found")
			return
		}
		name := strings.TrimSpace(b.DisplayName)
		if name == "" {
			name = userDisplayName(ctx, tx, uid)
		}
		var ownerMember int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_pocket_members (owner_id, pocket_id, user_id, display_name, role)
			VALUES ($1,$2,$1,$3,'owner') RETURNING id`, uid, id, name).Scan(&ownerMember); err != nil {
			internalError(w, r, "insert owner member", err)
			return
		}

		// Expenses: one zero-debt shared expense per txn; the txn becomes its echo.
		rows, err := tx.QueryContext(ctx, `SELECT id, amount, description, note, txn_at
			FROM fin_transactions WHERE pocket_id = $1 AND user_id = $2 AND type = 'expense'`, id, uid)
		if err != nil {
			internalError(w, r, "load pocket txns", err)
			return
		}
		type txn struct {
			id         int64
			amount     float64
			desc, note string
			at         time.Time
		}
		var txns []txn
		for rows.Next() {
			var t txn
			rows.Scan(&t.id, &t.amount, &t.desc, &t.note, &t.at)
			txns = append(txns, t)
		}
		rows.Close()
		for _, t := range txns {
			var eid int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO fin_shared_expenses (owner_id, pocket_id, paid_by, amount, description, note, split, spent_at, created_by)
				VALUES ($1,$2,$3,$4,$5,$6,'exact',$7,$1) RETURNING id`,
				uid, id, ownerMember, t.amount, t.desc, t.note, t.at).Scan(&eid); err != nil {
				internalError(w, r, "convert pocket txn", err)
				return
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO fin_expense_shares (expense_id, member_id, amount) VALUES ($1,$2,$3)`,
				eid, ownerMember, t.amount); err != nil {
				internalError(w, r, "convert pocket share", err)
				return
			}
			if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET shared_expense_id = $1, pocket_id = NULL WHERE id = $2`,
				eid, t.id); err != nil {
				internalError(w, r, "link converted echo", err)
				return
			}
		}
		// Everything else (income) → General.
		tx.ExecContext(ctx, `UPDATE fin_transactions SET pocket_id = NULL WHERE pocket_id = $1 AND user_id = $2`, id, uid)

		logPocketActivity(ctx, tx, uid, id, name, "pocket_shared", map[string]any{"expenses": len(txns)})
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit pocket share", err)
			return
		}
		writeJSON(w, 200, map[string]any{"status": "ok", "member_id": ownerMember, "converted": len(txns)})
	}
}

// --- detail / members ----------------------------------------------------------

type pocketMemberResp struct {
	ID           int64  `json:"id"`
	DisplayName  string `json:"display_name"`
	Role         string `json:"role"`
	IsRegistered bool   `json:"is_registered"`
	IsMe         bool   `json:"is_me"`
	Left         bool   `json:"left"`
	Email        string `json:"email,omitempty"` // owner-only
}

func loadPocketMembers(ctx context.Context, d *db.DB, uid string, pocketID int64, includeEmails bool) ([]pocketMemberResp, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT m.id, m.display_name, m.role, m.user_id IS NOT NULL,
		       COALESCE(m.user_id::text,'') = $2, m.left_at IS NOT NULL,
		       COALESCE(i.email::text, u.email::text, '')
		FROM fin_pocket_members m
		LEFT JOIN users u ON u.id = m.user_id
		LEFT JOIN LATERAL (
			SELECT email FROM fin_pocket_invites WHERE member_id = m.id ORDER BY created_at DESC LIMIT 1
		) i ON TRUE
		WHERE m.pocket_id = $1
		ORDER BY m.created_at ASC`, pocketID, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pocketMemberResp{}
	for rows.Next() {
		var m pocketMemberResp
		var email string
		rows.Scan(&m.ID, &m.DisplayName, &m.Role, &m.IsRegistered, &m.IsMe, &m.Left, &email)
		if includeEmails {
			m.Email = email
		}
		out = append(out, m)
	}
	return out, nil
}

func getPocketDetail(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		ctx := r.Context()
		a, err := requirePocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "pocket detail", err)
			return
		}
		var name, color, kind string
		var archived bool
		d.QueryRowContext(ctx, `SELECT name, color, kind, archived FROM fin_pockets WHERE id = $1`, id).
			Scan(&name, &color, &kind, &archived)

		out := map[string]any{
			"id": id, "name": name, "color": color, "kind": kind,
			"archived": archived, "is_owner": a.IsOwner, "my_member_id": a.MemberID,
		}
		if kind == "shared" {
			members, err := loadPocketMembers(ctx, d, uid, id, a.IsOwner)
			if err != nil {
				internalError(w, r, "load pocket members", err)
				return
			}
			out["members"] = members
			if a.IsOwner {
				invites := []map[string]any{}
				rows, _ := d.QueryContext(ctx, `SELECT id, member_id, email::text, status, expires_at
					FROM fin_pocket_invites WHERE pocket_id = $1 AND status = 'pending' ORDER BY created_at DESC`, id)
				if rows != nil {
					for rows.Next() {
						var iid, mid int64
						var email, status string
						var exp time.Time
						rows.Scan(&iid, &mid, &email, &status, &exp)
						invites = append(invites, map[string]any{
							"id": iid, "member_id": mid, "email": email,
							"expired": time.Now().After(exp), "expires_at": exp.Format(time.RFC3339),
						})
					}
					rows.Close()
				}
				out["invites"] = invites
			}
		}
		writeJSON(w, 200, out)
	}
}

// addPocketMember adds a text-only participant (any member may).
func addPocketMember(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			DisplayName string `json:"display_name"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		name := strings.TrimSpace(b.DisplayName)
		if name == "" {
			errJSON(w, 400, "display_name required")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "add pocket member", err)
			return
		}
		var dup int
		d.QueryRowContext(ctx, `SELECT 1 FROM fin_pocket_members WHERE pocket_id = $1 AND LOWER(display_name) = LOWER($2) AND left_at IS NULL`,
			id, name).Scan(&dup)
		if dup == 1 {
			errJSON(w, 400, "a person with that name is already in this pocket")
			return
		}
		var mid int64
		if err := d.QueryRowContext(ctx, `INSERT INTO fin_pocket_members (owner_id, pocket_id, display_name) VALUES ($1,$2,$3) RETURNING id`,
			a.OwnerID, id, name).Scan(&mid); err != nil {
			internalError(w, r, "insert pocket member", err)
			return
		}
		logPocketActivity(ctx, d, a.OwnerID, id, a.MemberName, "member_added", map[string]any{"name": name})
		writeJSON(w, 201, map[string]int64{"id": mid})
	}
}

// renamePocketMember: owner may rename anyone; a member may rename themselves.
func renamePocketMember(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		mid, err := intParam(r, "mid")
		if err != nil {
			errJSON(w, 400, "invalid member id")
			return
		}
		var b struct {
			DisplayName string `json:"display_name"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		name := strings.TrimSpace(b.DisplayName)
		if name == "" {
			errJSON(w, 400, "display_name required")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "rename pocket member", err)
			return
		}
		if !a.IsOwner && a.MemberID != mid {
			errJSON(w, 403, "only the owner can rename other people")
			return
		}
		res, err := d.ExecContext(ctx, `UPDATE fin_pocket_members SET display_name = $1 WHERE id = $2 AND pocket_id = $3`, name, mid, id)
		if err != nil {
			internalError(w, r, "rename pocket member", err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			errJSON(w, 404, "not found")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// memberNetPaise computes one member's net balance in paise.
// net = paid − share + settled-out − settled-in; positive = is owed.
func memberNetPaise(ctx context.Context, q dbtx, memberID int64) (int64, error) {
	var net int64
	err := q.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT SUM(ROUND(amount*100)::bigint) FROM fin_shared_expenses WHERE paid_by = $1),0)
		- COALESCE((SELECT SUM(ROUND(amount*100)::bigint) FROM fin_expense_shares WHERE member_id = $1),0)
		+ COALESCE((SELECT SUM(ROUND(amount*100)::bigint) FROM fin_pocket_settlements WHERE from_member = $1),0)
		- COALESCE((SELECT SUM(ROUND(amount*100)::bigint) FROM fin_pocket_settlements WHERE to_member = $1),0)`,
		memberID).Scan(&net)
	return net, err
}

// departPocketMember implements both remove (owner) and leave (self):
// zero-balance guard, then soft-leave when history references the member,
// hard delete otherwise.
func departPocketMember(ctx context.Context, d *db.DB, a *pocketAccess, pocketID, mid int64, kind string) (int, string) {
	var role string
	var isMember bool
	if err := d.QueryRowContext(ctx, `SELECT role, left_at IS NULL FROM fin_pocket_members WHERE id = $1 AND pocket_id = $2`,
		mid, pocketID).Scan(&role, &isMember); err != nil {
		return 404, "not found"
	}
	if role == "owner" {
		return 400, "the owner cannot be removed"
	}
	if !isMember {
		return 400, "already left"
	}
	net, err := memberNetPaise(ctx, d, mid)
	if err != nil {
		return 500, err.Error()
	}
	if net != 0 {
		return 400, "settle up first — this person's balance is not zero"
	}
	var name string
	d.QueryRowContext(ctx, `SELECT display_name FROM fin_pocket_members WHERE id = $1`, mid).Scan(&name)
	var referenced bool
	d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fin_shared_expenses WHERE paid_by = $1)
		OR EXISTS(SELECT 1 FROM fin_expense_shares WHERE member_id = $1)
		OR EXISTS(SELECT 1 FROM fin_pocket_settlements WHERE from_member = $1 OR to_member = $1)`,
		mid).Scan(&referenced)
	if referenced {
		if _, err := d.ExecContext(ctx, `UPDATE fin_pocket_members SET left_at = NOW() WHERE id = $1`, mid); err != nil {
			return 500, err.Error()
		}
	} else {
		if _, err := d.ExecContext(ctx, `DELETE FROM fin_pocket_members WHERE id = $1`, mid); err != nil {
			return 500, err.Error()
		}
		// A pending invite for this member is gone with it (FK CASCADE).
	}
	logPocketActivity(ctx, d, a.OwnerID, pocketID, a.MemberName, kind, map[string]any{"name": name})
	return 200, ""
}

func removePocketMember(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		mid, err := intParam(r, "mid")
		if err != nil {
			errJSON(w, 400, "invalid member id")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "remove pocket member", err)
			return
		}
		if !a.IsOwner {
			errJSON(w, 403, "only the owner can remove people")
			return
		}
		if status, msg := departPocketMember(ctx, d, a, id, mid, "member_removed"); status != 200 {
			errJSON(w, status, msg)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func leavePocket(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "leave pocket", err)
			return
		}
		if a.IsOwner {
			errJSON(w, 400, "the owner cannot leave — delete the pocket instead")
			return
		}
		if status, msg := departPocketMember(ctx, d, a, id, a.MemberID, "member_left"); status != 200 {
			errJSON(w, status, msg)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- expenses ------------------------------------------------------------------

type expenseShareResp struct {
	MemberID    int64   `json:"member_id"`
	DisplayName string  `json:"display_name"`
	Amount      float64 `json:"amount"`
}

type sharedExpenseResp struct {
	ID            int64              `json:"id"`
	Description   string             `json:"description"`
	Note          string             `json:"note"`
	Amount        float64            `json:"amount"`
	Split         string             `json:"split"`
	SpentAt       string             `json:"spent_at"`
	PaidBy        int64              `json:"paid_by"`
	PaidByName    string             `json:"paid_by_name"`
	CreatedByName string             `json:"created_by_name"`
	Shares        []expenseShareResp `json:"shares"`
	MyEchoTxnID   *int64             `json:"my_echo_txn_id"`
}

func listPocketExpenses(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		ctx := r.Context()
		if _, err := requireSharedPocketAccess(ctx, d, uid, id); err != nil {
			pocketAccessError(w, r, "list pocket expenses", err)
			return
		}
		loc := userLocation(d, uid)
		rows, err := d.QueryContext(ctx, `
			SELECT e.id, e.description, e.note, e.amount, e.split, e.spent_at,
			       e.paid_by, pm.display_name,
			       COALESCE(cm.display_name, ''), t.id
			FROM fin_shared_expenses e
			JOIN fin_pocket_members pm ON pm.id = e.paid_by
			LEFT JOIN fin_pocket_members cm ON cm.pocket_id = e.pocket_id AND cm.user_id = e.created_by
			LEFT JOIN fin_transactions t ON t.shared_expense_id = e.id AND t.user_id = $2
			WHERE e.pocket_id = $1
			ORDER BY e.spent_at DESC, e.id DESC`, id, uid)
		if err != nil {
			internalError(w, r, "list pocket expenses", err)
			return
		}
		defer rows.Close()
		out := []sharedExpenseResp{}
		idx := map[int64]int{}
		var ids []int64
		for rows.Next() {
			var e sharedExpenseResp
			var at time.Time
			rows.Scan(&e.ID, &e.Description, &e.Note, &e.Amount, &e.Split, &at,
				&e.PaidBy, &e.PaidByName, &e.CreatedByName, &e.MyEchoTxnID)
			e.SpentAt = at.In(loc).Format(time.RFC3339)
			e.Shares = []expenseShareResp{}
			idx[e.ID] = len(out)
			ids = append(ids, e.ID)
			out = append(out, e)
		}
		if len(ids) > 0 {
			srows, serr := d.QueryContext(ctx, `
				SELECT s.expense_id, s.member_id, m.display_name, s.amount
				FROM fin_expense_shares s JOIN fin_pocket_members m ON m.id = s.member_id
				WHERE s.expense_id = ANY($1) ORDER BY s.member_id`, ids)
			if serr == nil {
				for srows.Next() {
					var eid int64
					var s expenseShareResp
					srows.Scan(&eid, &s.MemberID, &s.DisplayName, &s.Amount)
					out[idx[eid]].Shares = append(out[idx[eid]].Shares, s)
				}
				srows.Close()
			}
		}
		writeJSON(w, 200, out)
	}
}

// resolveShares validates and materializes split shares in paise.
// equal: total split evenly over the members, remainder paise to the first
// members ordered by id (deterministic). exact: amounts must sum to total.
func resolveShares(split string, totalPaise int64, shares []struct {
	MemberID int64   `json:"member_id"`
	Amount   float64 `json:"amount"`
}) (map[int64]int64, string) {
	if len(shares) == 0 {
		return nil, "at least one participant required"
	}
	ids := make([]int64, 0, len(shares))
	seen := map[int64]bool{}
	for _, s := range shares {
		if seen[s.MemberID] {
			return nil, "duplicate participant"
		}
		seen[s.MemberID] = true
		ids = append(ids, s.MemberID)
	}
	out := map[int64]int64{}
	switch split {
	case "equal":
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		n := int64(len(ids))
		base := totalPaise / n
		rem := totalPaise % n
		for i, mid := range ids {
			out[mid] = base
			if int64(i) < rem {
				out[mid]++
			}
		}
	case "exact":
		var sum int64
		for _, s := range shares {
			p := paise(s.Amount)
			if p < 0 {
				return nil, "share amounts cannot be negative"
			}
			out[s.MemberID] = p
			sum += p
		}
		if sum != totalPaise {
			return nil, "shares must add up to the amount"
		}
	default:
		return nil, "split must be equal or exact"
	}
	return out, ""
}

// requirePocketParticipants verifies every id is an active member of the pocket.
func requirePocketParticipants(ctx context.Context, q dbtx, pocketID int64, ids []int64) error {
	// Dedupe: callers pass paid_by alongside the share members, and the payer
	// is usually one of them — ANY() matches rows once, so duplicates would
	// break the count comparison.
	uniq := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	var ok bool
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) = cardinality($2::bigint[])
		FROM fin_pocket_members WHERE pocket_id = $1 AND id = ANY($2) AND left_at IS NULL`,
		pocketID, uniq).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errFinanceReferenceNotFound
	}
	return nil
}

type expenseEcho struct {
	AccountID  int64  `json:"account_id"`
	CategoryID *int64 `json:"category_id"`
}

// insertExpenseEcho writes the payer's personal-ledger row for a shared
// expense. Caller must have verified actor == payer and account ownership.
func insertExpenseEcho(ctx context.Context, q dbtx, uid string, e expenseEcho, expenseID int64, amount float64, desc string, at time.Time) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at, shared_expense_id)
		VALUES ($1,$2,$3,'expense',$4,$5,$6,$7) RETURNING id`,
		uid, e.AccountID, e.CategoryID, amount, desc, at, expenseID).Scan(&id)
	return id, err
}

func createPocketExpense(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			Amount      float64 `json:"amount"`
			Description string  `json:"description"`
			Note        string  `json:"note"`
			SpentAt     string  `json:"spent_at"`
			PaidBy      int64   `json:"paid_by"`
			Split       string  `json:"split"`
			Shares      []struct {
				MemberID int64   `json:"member_id"`
				Amount   float64 `json:"amount"`
			} `json:"shares"`
			Echo *expenseEcho `json:"echo"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Amount <= 0 {
			errJSON(w, 400, "amount must be positive")
			return
		}
		if b.Split == "" {
			b.Split = "equal"
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "create pocket expense", err)
			return
		}
		shareMap, msg := resolveShares(b.Split, paise(b.Amount), b.Shares)
		if msg != "" {
			errJSON(w, 400, msg)
			return
		}
		participants := append([]int64{b.PaidBy}, mapKeys(shareMap)...)
		if err := requirePocketParticipants(ctx, d, id, participants); err != nil {
			pocketAccessError(w, r, "validate expense participants", err)
			return
		}
		spentAt := resolveTxnAt(b.SpentAt, userNow(d, uid))

		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin pocket expense", err)
			return
		}
		defer tx.Rollback()
		var eid int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_shared_expenses (owner_id, pocket_id, paid_by, amount, description, note, split, spent_at, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
			a.OwnerID, id, b.PaidBy, b.Amount, b.Description, b.Note, b.Split, spentAt, uid).Scan(&eid); err != nil {
			internalError(w, r, "insert pocket expense", err)
			return
		}
		for mid, p := range shareMap {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fin_expense_shares (expense_id, member_id, amount) VALUES ($1,$2,$3)`,
				eid, mid, rupees(p)); err != nil {
				internalError(w, r, "insert expense share", err)
				return
			}
		}
		var echoTxn *int64
		if b.Echo != nil && b.PaidBy == a.MemberID {
			if err := requireOwnedFinanceRef(ctx, tx, "fin_accounts", uid, b.Echo.AccountID); err != nil {
				pocketAccessError(w, r, "validate echo account", err)
				return
			}
			if b.Echo.CategoryID != nil {
				if err := requireOwnedFinanceRef(ctx, tx, "fin_categories", uid, *b.Echo.CategoryID); err != nil {
					pocketAccessError(w, r, "validate echo category", err)
					return
				}
			}
			tid, err := insertExpenseEcho(ctx, tx, uid, *b.Echo, eid, b.Amount, b.Description, spentAt)
			if err != nil {
				internalError(w, r, "insert expense echo", err)
				return
			}
			echoTxn = &tid
		}
		logPocketActivity(ctx, tx, a.OwnerID, id, a.MemberName, "expense_added",
			map[string]any{"description": b.Description, "amount": b.Amount})
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit pocket expense", err)
			return
		}
		writeJSON(w, 201, map[string]any{"id": eid, "echo_txn_id": echoTxn})
	}
}

func mapKeys(m map[int64]int64) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func updatePocketExpense(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		eid, err := intParam(r, "eid")
		if err != nil {
			errJSON(w, 400, "invalid expense id")
			return
		}
		var b struct {
			Amount      *float64 `json:"amount"`
			Description *string  `json:"description"`
			Note        *string  `json:"note"`
			SpentAt     *string  `json:"spent_at"`
			PaidBy      *int64   `json:"paid_by"`
			Split       *string  `json:"split"`
			Shares      *[]struct {
				MemberID int64   `json:"member_id"`
				Amount   float64 `json:"amount"`
			} `json:"shares"`
			Echo *expenseEcho `json:"echo"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "update pocket expense", err)
			return
		}

		var cur struct {
			amount float64
			desc   string
			split  string
			paidBy int64
			at     time.Time
		}
		if err := d.QueryRowContext(ctx, `SELECT amount, description, split, paid_by, spent_at
			FROM fin_shared_expenses WHERE id = $1 AND pocket_id = $2`, eid, id).
			Scan(&cur.amount, &cur.desc, &cur.split, &cur.paidBy, &cur.at); err != nil {
			errJSON(w, 404, "not found")
			return
		}

		amount := cur.amount
		if b.Amount != nil {
			if *b.Amount <= 0 {
				errJSON(w, 400, "amount must be positive")
				return
			}
			amount = *b.Amount
		}
		desc := cur.desc
		if b.Description != nil {
			desc = *b.Description
		}
		spentAt := cur.at
		if b.SpentAt != nil {
			spentAt = resolveTxnAt(*b.SpentAt, userNow(d, uid))
		}
		paidBy := cur.paidBy
		if b.PaidBy != nil {
			paidBy = *b.PaidBy
		}
		split := cur.split
		if b.Split != nil {
			split = *b.Split
		}

		// Splits are respecified when anything affecting them changes.
		// Equal splits with no shares in the body re-derive over the
		// current participant set.
		respec := b.Amount != nil || b.Split != nil || b.Shares != nil
		var shareMap map[int64]int64
		if respec {
			var shares []struct {
				MemberID int64   `json:"member_id"`
				Amount   float64 `json:"amount"`
			}
			if b.Shares != nil {
				shares = *b.Shares
			} else {
				rows, rerr := d.QueryContext(ctx, `SELECT member_id FROM fin_expense_shares WHERE expense_id = $1`, eid)
				if rerr != nil {
					internalError(w, r, "load expense shares", rerr)
					return
				}
				for rows.Next() {
					var mid int64
					rows.Scan(&mid)
					shares = append(shares, struct {
						MemberID int64   `json:"member_id"`
						Amount   float64 `json:"amount"`
					}{MemberID: mid})
				}
				rows.Close()
				if split == "exact" && b.Shares == nil {
					errJSON(w, 400, "exact split requires shares")
					return
				}
			}
			var msg string
			shareMap, msg = resolveShares(split, paise(amount), shares)
			if msg != "" {
				errJSON(w, 400, msg)
				return
			}
		}
		participants := []int64{paidBy}
		if shareMap != nil {
			participants = append(participants, mapKeys(shareMap)...)
		}
		if err := requirePocketParticipants(ctx, d, id, participants); err != nil {
			pocketAccessError(w, r, "validate expense participants", err)
			return
		}

		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin expense edit", err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `UPDATE fin_shared_expenses
			SET amount = $1, description = $2, note = COALESCE($3, note), split = $4, paid_by = $5, spent_at = $6, updated_at = NOW()
			WHERE id = $7`, amount, desc, b.Note, split, paidBy, spentAt, eid); err != nil {
			internalError(w, r, "update pocket expense", err)
			return
		}
		if shareMap != nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM fin_expense_shares WHERE expense_id = $1`, eid); err != nil {
				internalError(w, r, "replace expense shares", err)
				return
			}
			for mid, p := range shareMap {
				if _, err := tx.ExecContext(ctx, `INSERT INTO fin_expense_shares (expense_id, member_id, amount) VALUES ($1,$2,$3)`,
					eid, mid, rupees(p)); err != nil {
					internalError(w, r, "replace expense share", err)
					return
				}
			}
		}

		// Echo sync: payer change orphans the old payer's echo; otherwise the
		// linked row (whoever's ledger it lives in) follows the expense.
		if paidBy != cur.paidBy {
			if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE shared_expense_id = $1`, eid); err != nil {
				internalError(w, r, "drop stale expense echo", err)
				return
			}
			if b.Echo != nil && paidBy == a.MemberID {
				if err := requireOwnedFinanceRef(ctx, tx, "fin_accounts", uid, b.Echo.AccountID); err != nil {
					pocketAccessError(w, r, "validate echo account", err)
					return
				}
				if _, err := insertExpenseEcho(ctx, tx, uid, *b.Echo, eid, amount, desc, spentAt); err != nil {
					internalError(w, r, "insert expense echo", err)
					return
				}
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET amount = $1, description = $2, txn_at = $3, updated_at = NOW()
				WHERE shared_expense_id = $4`, amount, desc, spentAt, eid); err != nil {
				internalError(w, r, "sync expense echo", err)
				return
			}
		}
		logPocketActivity(ctx, tx, a.OwnerID, id, a.MemberName, "expense_updated",
			map[string]any{"description": desc, "amount": amount})
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit expense edit", err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deletePocketExpense(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		eid, err := intParam(r, "eid")
		if err != nil {
			errJSON(w, 400, "invalid expense id")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "delete pocket expense", err)
			return
		}
		var desc string
		var amount float64
		if err := d.QueryRowContext(ctx, `SELECT description, amount FROM fin_shared_expenses WHERE id = $1 AND pocket_id = $2`,
			eid, id).Scan(&desc, &amount); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin expense delete", err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE shared_expense_id = $1`, eid); err != nil {
			internalError(w, r, "delete expense echo", err)
			return
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_shared_expenses WHERE id = $1`, eid); err != nil {
			internalError(w, r, "delete pocket expense", err)
			return
		}
		logPocketActivity(ctx, tx, a.OwnerID, id, a.MemberName, "expense_deleted",
			map[string]any{"description": desc, "amount": amount})
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit expense delete", err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// attachExpenseEcho lets the payer add an expense to their own ledger after
// the fact (e.g. someone else recorded it).
func attachExpenseEcho(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		eid, err := intParam(r, "eid")
		if err != nil {
			errJSON(w, 400, "invalid expense id")
			return
		}
		var b expenseEcho
		if err := readJSON(r, &b); err != nil || b.AccountID == 0 {
			errJSON(w, 400, "account_id required")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "attach expense echo", err)
			return
		}
		var paidBy int64
		var amount float64
		var desc string
		var at time.Time
		if err := d.QueryRowContext(ctx, `SELECT paid_by, amount, description, spent_at
			FROM fin_shared_expenses WHERE id = $1 AND pocket_id = $2`, eid, id).
			Scan(&paidBy, &amount, &desc, &at); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		if paidBy != a.MemberID {
			errJSON(w, 403, "only the payer can add this to their ledger")
			return
		}
		var exists bool
		d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fin_transactions WHERE shared_expense_id = $1)`, eid).Scan(&exists)
		if exists {
			errJSON(w, 400, "already on your ledger")
			return
		}
		if err := requireOwnedFinanceRef(ctx, d, "fin_accounts", uid, b.AccountID); err != nil {
			pocketAccessError(w, r, "validate echo account", err)
			return
		}
		if b.CategoryID != nil {
			if err := requireOwnedFinanceRef(ctx, d, "fin_categories", uid, *b.CategoryID); err != nil {
				pocketAccessError(w, r, "validate echo category", err)
				return
			}
		}
		tid, err := insertExpenseEcho(ctx, d, uid, b, eid, amount, desc, at)
		if err != nil {
			internalError(w, r, "insert expense echo", err)
			return
		}
		writeJSON(w, 201, map[string]int64{"txn_id": tid})
	}
}

// --- balances ------------------------------------------------------------------

type settleSuggestion struct {
	FromMember int64   `json:"from_member"`
	ToMember   int64   `json:"to_member"`
	Amount     float64 `json:"amount"`
}

// simplifySettlements reduces net balances (paise, keyed by member) to at
// most n−1 transfers: largest debtor pays largest creditor, repeat.
// Deterministic: ties break on member id.
func simplifySettlements(nets map[int64]int64) []settleSuggestion {
	type entry struct {
		id  int64
		amt int64
	}
	var debtors, creditors []entry
	for id, n := range nets {
		if n < 0 {
			debtors = append(debtors, entry{id, -n})
		} else if n > 0 {
			creditors = append(creditors, entry{id, n})
		}
	}
	less := func(s []entry) func(int, int) bool {
		return func(i, j int) bool {
			if s[i].amt != s[j].amt {
				return s[i].amt > s[j].amt
			}
			return s[i].id < s[j].id
		}
	}
	sort.Slice(debtors, less(debtors))
	sort.Slice(creditors, less(creditors))
	out := []settleSuggestion{}
	i, j := 0, 0
	for i < len(debtors) && j < len(creditors) {
		amt := min(debtors[i].amt, creditors[j].amt)
		if amt > 0 {
			out = append(out, settleSuggestion{
				FromMember: debtors[i].id, ToMember: creditors[j].id, Amount: rupees(amt),
			})
		}
		debtors[i].amt -= amt
		creditors[j].amt -= amt
		if debtors[i].amt == 0 {
			i++
		}
		if creditors[j].amt == 0 {
			j++
		}
	}
	return out
}

// pocketNetPaise computes every member's net balance for a pocket in paise.
func pocketNetPaise(ctx context.Context, d *db.DB, pocketID int64) (map[int64]int64, error) {
	nets := map[int64]int64{}
	rows, err := d.QueryContext(ctx, `
		SELECT m.id,
			COALESCE((SELECT SUM(ROUND(e.amount*100)::bigint) FROM fin_shared_expenses e WHERE e.paid_by = m.id),0)
			- COALESCE((SELECT SUM(ROUND(s.amount*100)::bigint) FROM fin_expense_shares s WHERE s.member_id = m.id),0)
			+ COALESCE((SELECT SUM(ROUND(st.amount*100)::bigint) FROM fin_pocket_settlements st WHERE st.from_member = m.id),0)
			- COALESCE((SELECT SUM(ROUND(st.amount*100)::bigint) FROM fin_pocket_settlements st WHERE st.to_member = m.id),0)
		FROM fin_pocket_members m WHERE m.pocket_id = $1`, pocketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, net int64
		rows.Scan(&id, &net)
		nets[id] = net
	}
	return nets, nil
}

func pocketBalances(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		ctx := r.Context()
		if _, err := requireSharedPocketAccess(ctx, d, uid, id); err != nil {
			pocketAccessError(w, r, "pocket balances", err)
			return
		}
		nets, err := pocketNetPaise(ctx, d, id)
		if err != nil {
			internalError(w, r, "pocket balances", err)
			return
		}
		type memberBalance struct {
			MemberID    int64   `json:"member_id"`
			DisplayName string  `json:"display_name"`
			IsMe        bool    `json:"is_me"`
			Left        bool    `json:"left"`
			Paid        float64 `json:"paid"`
			Share       float64 `json:"share"`
			Net         float64 `json:"net"`
		}
		out := []memberBalance{}
		rows, err := d.QueryContext(ctx, `
			SELECT m.id, m.display_name, COALESCE(m.user_id::text,'') = $2, m.left_at IS NOT NULL,
				COALESCE((SELECT SUM(e.amount) FROM fin_shared_expenses e WHERE e.paid_by = m.id),0),
				COALESCE((SELECT SUM(s.amount) FROM fin_expense_shares s WHERE s.member_id = m.id),0)
			FROM fin_pocket_members m WHERE m.pocket_id = $1 ORDER BY m.created_at ASC`, id, uid)
		if err != nil {
			internalError(w, r, "pocket balances", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var m memberBalance
			rows.Scan(&m.MemberID, &m.DisplayName, &m.IsMe, &m.Left, &m.Paid, &m.Share)
			m.Net = rupees(nets[m.MemberID])
			// A departed member with a settled balance is history, not a balance row.
			if m.Left && nets[m.MemberID] == 0 {
				continue
			}
			out = append(out, m)
		}
		writeJSON(w, 200, map[string]any{
			"members":     out,
			"suggestions": simplifySettlements(nets),
		})
	}
}

// --- settlements ----------------------------------------------------------------

func listPocketSettlements(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		ctx := r.Context()
		if _, err := requireSharedPocketAccess(ctx, d, uid, id); err != nil {
			pocketAccessError(w, r, "list settlements", err)
			return
		}
		loc := userLocation(d, uid)
		type settlementResp struct {
			ID          int64   `json:"id"`
			FromMember  int64   `json:"from_member"`
			FromName    string  `json:"from_name"`
			ToMember    int64   `json:"to_member"`
			ToName      string  `json:"to_name"`
			Amount      float64 `json:"amount"`
			SettledAt   string  `json:"settled_at"`
			MyEchoTxnID *int64  `json:"my_echo_txn_id"`
		}
		rows, err := d.QueryContext(ctx, `
			SELECT s.id, s.from_member, fm.display_name, s.to_member, tm.display_name, s.amount, s.settled_at, t.id
			FROM fin_pocket_settlements s
			JOIN fin_pocket_members fm ON fm.id = s.from_member
			JOIN fin_pocket_members tm ON tm.id = s.to_member
			LEFT JOIN fin_transactions t ON t.settlement_id = s.id AND t.user_id = $2
			WHERE s.pocket_id = $1 ORDER BY s.settled_at DESC, s.id DESC`, id, uid)
		if err != nil {
			internalError(w, r, "list settlements", err)
			return
		}
		defer rows.Close()
		out := []settlementResp{}
		for rows.Next() {
			var s settlementResp
			var at time.Time
			rows.Scan(&s.ID, &s.FromMember, &s.FromName, &s.ToMember, &s.ToName, &s.Amount, &at, &s.MyEchoTxnID)
			s.SettledAt = at.In(loc).Format(time.RFC3339)
			out = append(out, s)
		}
		writeJSON(w, 200, out)
	}
}

// insertSettlementEcho writes the acting party's own leg of a settlement to
// their ledger: money they paid out = expense, money received = income.
func insertSettlementEcho(ctx context.Context, q dbtx, uid string, e expenseEcho, settlementID int64, txnType string, amount float64, desc string, at time.Time) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at, settlement_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		uid, e.AccountID, e.CategoryID, txnType, amount, desc, at, settlementID).Scan(&id)
	return id, err
}

func createPocketSettlement(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			FromMember int64        `json:"from_member"`
			ToMember   int64        `json:"to_member"`
			Amount     float64      `json:"amount"`
			SettledAt  string       `json:"settled_at"`
			Echo       *expenseEcho `json:"echo"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Amount <= 0 {
			errJSON(w, 400, "amount must be positive")
			return
		}
		if b.FromMember == b.ToMember {
			errJSON(w, 400, "from and to must differ")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "create settlement", err)
			return
		}
		if err := requirePocketParticipants(ctx, d, id, []int64{b.FromMember, b.ToMember}); err != nil {
			pocketAccessError(w, r, "validate settlement members", err)
			return
		}
		settledAt := resolveTxnAt(b.SettledAt, userNow(d, uid))

		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin settlement", err)
			return
		}
		defer tx.Rollback()
		var sid int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_pocket_settlements (owner_id, pocket_id, from_member, to_member, amount, settled_at, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			a.OwnerID, id, b.FromMember, b.ToMember, b.Amount, settledAt, uid).Scan(&sid); err != nil {
			internalError(w, r, "insert settlement", err)
			return
		}
		var fromName, toName string
		tx.QueryRowContext(ctx, `SELECT display_name FROM fin_pocket_members WHERE id = $1`, b.FromMember).Scan(&fromName)
		tx.QueryRowContext(ctx, `SELECT display_name FROM fin_pocket_members WHERE id = $1`, b.ToMember).Scan(&toName)

		var echoTxn *int64
		if b.Echo != nil && (a.MemberID == b.FromMember || a.MemberID == b.ToMember) {
			if err := requireOwnedFinanceRef(ctx, tx, "fin_accounts", uid, b.Echo.AccountID); err != nil {
				pocketAccessError(w, r, "validate echo account", err)
				return
			}
			if b.Echo.CategoryID != nil {
				if err := requireOwnedFinanceRef(ctx, tx, "fin_categories", uid, *b.Echo.CategoryID); err != nil {
					pocketAccessError(w, r, "validate echo category", err)
					return
				}
			}
			txnType, desc := "expense", "Settlement to "+toName
			if a.MemberID == b.ToMember {
				txnType, desc = "income", "Settlement from "+fromName
			}
			tid, err := insertSettlementEcho(ctx, tx, uid, *b.Echo, sid, txnType, b.Amount, desc+" · "+a.Name, settledAt)
			if err != nil {
				internalError(w, r, "insert settlement echo", err)
				return
			}
			echoTxn = &tid
		}
		logPocketActivity(ctx, tx, a.OwnerID, id, a.MemberName, "settlement_added",
			map[string]any{"from": fromName, "to": toName, "amount": b.Amount})
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit settlement", err)
			return
		}
		writeJSON(w, 201, map[string]any{"id": sid, "echo_txn_id": echoTxn})
	}
}

func deletePocketSettlement(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		sid, err := intParam(r, "sid")
		if err != nil {
			errJSON(w, 400, "invalid settlement id")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "delete settlement", err)
			return
		}
		var amount float64
		if err := d.QueryRowContext(ctx, `SELECT amount FROM fin_pocket_settlements WHERE id = $1 AND pocket_id = $2`,
			sid, id).Scan(&amount); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin settlement delete", err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE settlement_id = $1`, sid); err != nil {
			internalError(w, r, "delete settlement echoes", err)
			return
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_pocket_settlements WHERE id = $1`, sid); err != nil {
			internalError(w, r, "delete settlement", err)
			return
		}
		logPocketActivity(ctx, tx, a.OwnerID, id, a.MemberName, "settlement_deleted", map[string]any{"amount": amount})
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit settlement delete", err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// attachSettlementEcho lets the counterparty add their leg of a settlement
// to their own ledger (echoes are never auto-written into anyone's ledger).
func attachSettlementEcho(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		sid, err := intParam(r, "sid")
		if err != nil {
			errJSON(w, 400, "invalid settlement id")
			return
		}
		var b expenseEcho
		if err := readJSON(r, &b); err != nil || b.AccountID == 0 {
			errJSON(w, 400, "account_id required")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "attach settlement echo", err)
			return
		}
		var from, to int64
		var amount float64
		var at time.Time
		if err := d.QueryRowContext(ctx, `SELECT from_member, to_member, amount, settled_at
			FROM fin_pocket_settlements WHERE id = $1 AND pocket_id = $2`, sid, id).
			Scan(&from, &to, &amount, &at); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		if a.MemberID != from && a.MemberID != to {
			errJSON(w, 403, "you are not part of this settlement")
			return
		}
		var exists bool
		d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fin_transactions WHERE settlement_id = $1 AND user_id = $2)`,
			sid, uid).Scan(&exists)
		if exists {
			errJSON(w, 400, "already on your ledger")
			return
		}
		if err := requireOwnedFinanceRef(ctx, d, "fin_accounts", uid, b.AccountID); err != nil {
			pocketAccessError(w, r, "validate echo account", err)
			return
		}
		if b.CategoryID != nil {
			if err := requireOwnedFinanceRef(ctx, d, "fin_categories", uid, *b.CategoryID); err != nil {
				pocketAccessError(w, r, "validate echo category", err)
				return
			}
		}
		var otherName string
		txnType := "expense"
		other := to
		if a.MemberID == to {
			txnType = "income"
			other = from
		}
		d.QueryRowContext(ctx, `SELECT display_name FROM fin_pocket_members WHERE id = $1`, other).Scan(&otherName)
		desc := "Settlement to " + otherName
		if txnType == "income" {
			desc = "Settlement from " + otherName
		}
		tid, err := insertSettlementEcho(ctx, d, uid, b, sid, txnType, amount, desc+" · "+a.Name, at)
		if err != nil {
			internalError(w, r, "insert settlement echo", err)
			return
		}
		writeJSON(w, 201, map[string]int64{"txn_id": tid})
	}
}

// --- activity ------------------------------------------------------------------

func pocketActivity(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		ctx := r.Context()
		if _, err := requireSharedPocketAccess(ctx, d, uid, id); err != nil {
			pocketAccessError(w, r, "pocket activity", err)
			return
		}
		limit := 50
		if v := queryParam(r, "limit"); v != "" {
			if n, err := atoiBounded(v, 1, 200); err == nil {
				limit = n
			}
		}
		args := []any{id}
		q := `SELECT id, actor_name, kind, detail, created_at FROM fin_pocket_activity WHERE pocket_id = $1`
		if v := queryParam(r, "before_id"); v != "" {
			q += " AND id < $2"
			args = append(args, v)
		}
		q += " ORDER BY id DESC LIMIT " + itoa(limit)
		rows, err := d.QueryContext(ctx, q, args...)
		if err != nil {
			internalError(w, r, "pocket activity", err)
			return
		}
		defer rows.Close()
		type activityResp struct {
			ID        int64           `json:"id"`
			ActorName string          `json:"actor_name"`
			Kind      string          `json:"kind"`
			Detail    json.RawMessage `json:"detail"`
			CreatedAt string          `json:"created_at"`
		}
		loc := userLocation(d, uid)
		out := []activityResp{}
		for rows.Next() {
			var a activityResp
			var at time.Time
			rows.Scan(&a.ID, &a.ActorName, &a.Kind, &a.Detail, &at)
			a.CreatedAt = at.In(loc).Format(time.RFC3339)
			out = append(out, a)
		}
		writeJSON(w, 200, out)
	}
}

func atoiBounded(s string, lo, hi int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
		if n > hi {
			return hi, nil
		}
	}
	if n < lo {
		return lo, nil
	}
	return n, nil
}
