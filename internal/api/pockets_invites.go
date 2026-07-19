package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"sajni/internal/db"
)

// Shared-pocket invites. Inviting creates the member row immediately (you
// can split with someone before they accept); the invite only carries the
// claim token. Accepting requires being signed in with the invited email —
// then the member row gains user_id and the pocket appears in their account.
// Tokens: 32 random bytes, base64url in the emailed link, SHA-256 at rest
// (refresh_tokens pattern). Expiry 72h, checked at read/accept time.

const pocketInviteTTL = 72 * time.Hour

func registerPocketInviteRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /api/finance/pockets/{id}/invites", createPocketInvite(deps))
	mux.HandleFunc("DELETE /api/finance/pockets/{id}/invites/{iid}", revokePocketInvite(deps))
	mux.HandleFunc("GET /api/finance/pockets/invites", myPocketInvites(deps))
	// Token rides a query param: a {token} wildcard here would conflict with
	// the GET /pockets/{id}/... routes under Go 1.22 mux precedence rules.
	mux.HandleFunc("GET /api/finance/pockets/invites/preview", previewPocketInvite(deps))
	mux.HandleFunc("POST /api/finance/pockets/invites/accept", acceptPocketInvite(deps))
}

func hashInviteToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

type pocketInviteSummary struct {
	ID          int64  `json:"id"`
	PocketName  string `json:"pocket_name"`
	InviterName string `json:"inviter_name"`
	ExpiresAt   string `json:"expires_at"`
}

// listMyPocketInvites returns pending, unexpired invites addressed to the
// user's email — surfaced as banner cards on the Pockets tab.
func listMyPocketInvites(ctx context.Context, d *db.DB, uid string) []pocketInviteSummary {
	out := []pocketInviteSummary{}
	rows, err := d.QueryContext(ctx, `
		SELECT i.id, p.name, COALESCE(om.display_name, ''), i.expires_at
		FROM fin_pocket_invites i
		JOIN users u ON u.id = $1 AND u.email = i.email
		JOIN fin_pockets p ON p.id = i.pocket_id
		LEFT JOIN fin_pocket_members om ON om.pocket_id = p.id AND om.role = 'owner'
		WHERE i.status = 'pending' AND i.expires_at > NOW()
		ORDER BY i.created_at DESC`, uid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s pocketInviteSummary
		var exp time.Time
		rows.Scan(&s.ID, &s.PocketName, &s.InviterName, &exp)
		s.ExpiresAt = exp.Format(time.RFC3339)
		out = append(out, s)
	}
	return out
}

func createPocketInvite(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
			MemberID    *int64 `json:"member_id"` // attach to an existing text-only person
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		addr, err := mail.ParseAddress(strings.TrimSpace(b.Email))
		if err != nil {
			errJSON(w, 400, "valid email required")
			return
		}
		email := strings.ToLower(addr.Address)
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "create pocket invite", err)
			return
		}
		if !a.IsOwner {
			errJSON(w, 403, "only the owner can invite people")
			return
		}

		// Soft rate-limit mirrors email_codes: max 3 sends per email per hour.
		var recent int
		d.QueryRowContext(ctx, `SELECT COUNT(*) FROM fin_pocket_invites WHERE email = $1 AND created_at > NOW() - INTERVAL '1 hour'`,
			email).Scan(&recent)
		if recent >= 3 {
			errJSON(w, 429, "too many invites to that email; try again later")
			return
		}
		var already bool
		d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fin_pocket_members m JOIN users u ON u.id = m.user_id
			WHERE m.pocket_id = $1 AND m.left_at IS NULL AND u.email = $2)`, id, email).Scan(&already)
		if already {
			errJSON(w, 400, "that person is already in this pocket")
			return
		}
		var pending bool
		d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fin_pocket_invites
			WHERE pocket_id = $1 AND email = $2 AND status = 'pending' AND expires_at > NOW())`, id, email).Scan(&pending)
		if pending {
			errJSON(w, 400, "an invite for that email is already pending")
			return
		}

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			internalError(w, r, "mint invite token", err)
			return
		}
		token := base64.RawURLEncoding.EncodeToString(raw)

		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin pocket invite", err)
			return
		}
		defer tx.Rollback()
		var mid int64
		if b.MemberID != nil {
			// Re-inviting / upgrading an existing text-only person.
			if err := tx.QueryRowContext(ctx, `SELECT id FROM fin_pocket_members
				WHERE id = $1 AND pocket_id = $2 AND user_id IS NULL AND left_at IS NULL`, *b.MemberID, id).Scan(&mid); err != nil {
				errJSON(w, 404, "not found")
				return
			}
		} else {
			name := strings.TrimSpace(b.DisplayName)
			if name == "" {
				name = strings.SplitN(email, "@", 2)[0]
			}
			if err := tx.QueryRowContext(ctx, `INSERT INTO fin_pocket_members (owner_id, pocket_id, display_name) VALUES ($1,$2,$3) RETURNING id`,
				a.OwnerID, id, name).Scan(&mid); err != nil {
				internalError(w, r, "insert invited member", err)
				return
			}
		}
		var iid int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_pocket_invites (owner_id, pocket_id, member_id, email, token_hash, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			a.OwnerID, id, mid, email, hashInviteToken(token), time.Now().Add(pocketInviteTTL)).Scan(&iid); err != nil {
			internalError(w, r, "insert pocket invite", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit pocket invite", err)
			return
		}

		acceptURL := strings.TrimRight(deps.Auth.AppURL, "/") + "/pockets/invite/" + token
		if err := deps.Auth.SendPocketInvite(ctx, email, a.MemberName, a.Name, acceptURL); err != nil {
			// Invite row stays valid — the owner can revoke and retry.
			errJSON(w, 502, "invite saved but the email failed to send: "+err.Error())
			return
		}
		if deps.Auth.DevLogEmailCodes && deps.Auth.ResendAPIKey == "" {
			fmt.Printf("[pockets/invite] (dev) accept URL for %s: %s\n", email, acceptURL)
		}
		writeJSON(w, 201, map[string]int64{"id": iid, "member_id": mid})
	}
}

func revokePocketInvite(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		iid, err := intParam(r, "iid")
		if err != nil {
			errJSON(w, 400, "invalid invite id")
			return
		}
		ctx := r.Context()
		a, err := requireSharedPocketAccess(ctx, d, uid, id)
		if err != nil {
			pocketAccessError(w, r, "revoke pocket invite", err)
			return
		}
		if !a.IsOwner {
			errJSON(w, 403, "only the owner can revoke invites")
			return
		}
		res, err := d.ExecContext(ctx, `UPDATE fin_pocket_invites SET status = 'revoked' WHERE id = $1 AND pocket_id = $2 AND status = 'pending'`,
			iid, id)
		if err != nil {
			internalError(w, r, "revoke pocket invite", err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			errJSON(w, 404, "not found")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func myPocketInvites(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		writeJSON(w, 200, listMyPocketInvites(r.Context(), d, uid))
	}
}

// previewPocketInvite renders the accept page: pocket + inviter, whether the
// invite is expired and whether it's addressed to the signed-in account. The
// invited email itself is never returned — token possession isn't proof of
// mailbox ownership.
func previewPocketInvite(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		token := r.URL.Query().Get("token")
		if token == "" {
			errJSON(w, 400, "invalid token")
			return
		}
		ctx := r.Context()
		var (
			pocketID              int64
			pocketName, status    string
			inviterName           string
			expiresAt             time.Time
			emailMatches, already bool
		)
		err := d.QueryRowContext(ctx, `
			SELECT p.id, p.name, COALESCE(om.display_name, ''), i.status, i.expires_at,
			       EXISTS(SELECT 1 FROM users u WHERE u.id = $2 AND u.email = i.email),
			       EXISTS(SELECT 1 FROM fin_pocket_members m WHERE m.pocket_id = p.id AND m.user_id = $2 AND m.left_at IS NULL)
			FROM fin_pocket_invites i
			JOIN fin_pockets p ON p.id = i.pocket_id
			LEFT JOIN fin_pocket_members om ON om.pocket_id = p.id AND om.role = 'owner'
			WHERE i.token_hash = $1`, hashInviteToken(token), uid).
			Scan(&pocketID, &pocketName, &inviterName, &status, &expiresAt, &emailMatches, &already)
		if errors.Is(err, sql.ErrNoRows) {
			errJSON(w, 404, "not found")
			return
		}
		if err != nil {
			internalError(w, r, "preview pocket invite", err)
			return
		}
		var memberCount int64
		d.QueryRowContext(ctx, `SELECT COUNT(*) FROM fin_pocket_members WHERE pocket_id = $1 AND left_at IS NULL`, pocketID).Scan(&memberCount)
		writeJSON(w, 200, map[string]any{
			"pocket_name":    pocketName,
			"inviter_name":   inviterName,
			"member_count":   memberCount,
			"status":         status,
			"expired":        status == "pending" && time.Now().After(expiresAt),
			"email_matches":  emailMatches,
			"already_member": already,
			"pocket_id":      pocketIDIfMember(already, pocketID),
		})
	}
}

// pocketIDIfMember only reveals the pocket id to someone already inside it.
func pocketIDIfMember(member bool, id int64) *int64 {
	if !member {
		return nil
	}
	return &id
}

func acceptPocketInvite(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			Token    string `json:"token"`
			InviteID *int64 `json:"invite_id"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin invite accept", err)
			return
		}
		defer tx.Rollback()

		// Locate the invite by token (email link) or id (in-app banner). Both
		// paths demand the signed-in account owns the invited email.
		q := `SELECT i.id, i.pocket_id, i.member_id, i.owner_id, i.status, i.expires_at,
		             EXISTS(SELECT 1 FROM users u WHERE u.id = $2 AND u.email = i.email)
		      FROM fin_pocket_invites i `
		var row *sql.Row
		switch {
		case b.Token != "":
			row = tx.QueryRowContext(ctx, q+`WHERE i.token_hash = $1 FOR UPDATE OF i`, hashInviteToken(b.Token), uid)
		case b.InviteID != nil:
			row = tx.QueryRowContext(ctx, q+`WHERE i.id = $1 FOR UPDATE OF i`, *b.InviteID, uid)
		default:
			errJSON(w, 400, "token or invite_id required")
			return
		}
		var (
			iid, pocketID, memberID int64
			ownerID, status         string
			expiresAt               time.Time
			emailMatches            bool
		)
		if err := row.Scan(&iid, &pocketID, &memberID, &ownerID, &status, &expiresAt, &emailMatches); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		if status != "pending" {
			errJSON(w, 400, "this invite is no longer active")
			return
		}
		if time.Now().After(expiresAt) {
			errJSON(w, 410, "this invite has expired — ask for a new one")
			return
		}
		if !emailMatches {
			errJSON(w, 403, "this invite was sent to a different email")
			return
		}
		var name string
		if err := tx.QueryRowContext(ctx, `UPDATE fin_pocket_members SET user_id = $1 WHERE id = $2 AND left_at IS NULL RETURNING display_name`,
			uid, memberID).Scan(&name); err != nil {
			// Unique (pocket_id,user_id) trips when they're already a member.
			errJSON(w, 400, "you're already in this pocket")
			return
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_pocket_invites SET status = 'accepted', accepted_at = NOW() WHERE id = $1`, iid); err != nil {
			internalError(w, r, "accept invite", err)
			return
		}
		logPocketActivity(ctx, tx, ownerID, pocketID, name, "member_joined", map[string]any{"name": name})
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit invite accept", err)
			return
		}
		writeJSON(w, 200, map[string]int64{"pocket_id": pocketID})
	}
}
