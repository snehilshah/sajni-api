package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/ai"
	"sajni/internal/db"
)

// captureParserSwapped is set by tests that replace captureParser.
var captureParserSwapped bool

// Capture with memory. The phone posts every bank SMS or app notification
// that looks like money moving; the server keeps each one (fin_captures) and
// decides, in one transaction under the per-user lock, whether it is a new
// payment, another copy of one already filed, the bank confirming something
// Sajni posted itself, or a question for the owner. Matching, in order:
//
//  1. The same message text again → absorbed.
//  2. The same bank/UPI reference as an earlier capture: same amount →
//     absorbed (even if that row was since deleted: the owner removed it on
//     purpose); a different amount → asked.
//  3. An RD/SIP instalment Sajni posted → confirmed; an investment maturing
//     → filed as money back (a transfer plus Interest).
//  4. One report per channel per payment: a message with no matching
//     reference, the same type and amount as a row within 3 hours that
//     this channel (the SMS inbox, or one app) hasn't reported yet, is that
//     row's copy from another channel → absorbed. Two real ₹22 payments
//     each bring their own SMS, so the second SMS finds the first row
//     already reported by "sms" and becomes its own row.
//  5. Otherwise a new row (or "review" when no account matched).
//
// "Add anyway" re-posts with force and skips 1, 2 and 4.

const (
	captureRefWindow  = 14 * 24 * time.Hour
	captureCopyWindow = 3 * time.Hour
)

type captureReq struct {
	Text        string `json:"text"`
	Source      string `json:"source"`       // sms | notification
	Sender      string `json:"sender"`       // SMS address or app package
	Channel     string `json:"channel"`      // finer than the package: mail apps send "package/from/subject"
	SenderLabel string `json:"sender_label"` // "bank SMS", "GPay", "Gmail"
	ReceivedAt  string `json:"received_at"`  // RFC3339; when the phone got it
	Ref         string `json:"ref"`          // the phone's own reading of the reference
	Force       bool   `json:"force"`        // "Add anyway"
}

type captureResp struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	TxnID   int64  `json:"txn_id,omitempty"`
	// Label names the row this message created or is a copy of
	// ("₹22.00 · Sewri"), for the notification.
	Label string `json:"label,omitempty"`
	// Parsed carries the parse-message fields so the phone can show or
	// pre-fill without a second call.
	Parsed   map[string]any `json:"parsed,omitempty"`
	Matured  string         `json:"matured,omitempty"`
	Interest float64        `json:"interest,omitempty"`
}

var captureSpaces = regexp.MustCompile(`\s+`)

func captureHash(text string) string {
	norm := captureSpaces.ReplaceAllString(strings.ToLower(strings.TrimSpace(text)), " ")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:16])
}

func normRef(ref string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(ref) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if !strings.ContainsAny(out, "0123456789") || len(out) < 6 {
		return ""
	}
	return out
}

// textMentionsAmount reports whether text states amount the way banks print
// it ("2724.00", "2,724", "Rs.2724").
func textMentionsAmount(text string, amount float64) bool {
	digits := strings.ReplaceAll(text, ",", "")
	forms := []string{fmt.Sprintf("%.2f", amount)}
	if whole := math.Round(amount); math.Abs(amount-whole) < 0.005 {
		forms = append(forms, fmt.Sprintf("%.0f", whole))
	}
	for _, f := range forms {
		if regexp.MustCompile(`(^|[^\d])` + regexp.QuoteMeta(f) + `($|[^\d])`).MatchString(digits) {
			return true
		}
	}
	return false
}

func moneyLabel(amount float64, desc string) string {
	l := "₹" + formatINR(amount)
	if desc = strings.TrimSpace(desc); desc != "" {
		l += " · " + desc
	}
	return l
}

// formatINR prints 2724 as "2,724.00" (Indian grouping).
func formatINR(v float64) string {
	s := fmt.Sprintf("%.2f", math.Abs(v))
	whole, frac := s[:len(s)-3], s[len(s)-3:]
	if len(whole) > 3 {
		head, tail := whole[:len(whole)-3], whole[len(whole)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		whole = strings.Join(parts, ",") + "," + tail
	}
	if v < 0 {
		return "-" + whole + frac
	}
	return whole + frac
}

// captureParser reads a message; tests swap it for a canned parse.
var captureParser = func(ctx context.Context, deps Deps, text, today string) (ai.ParsedTxn, int, error) {
	return deps.AI.ParseTransactionMessage(ctx, text, today, true)
}

func captureTransaction(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AI == nil && !captureParserSwapped {
			errJSON(w, http.StatusServiceUnavailable, "AI is not configured on this server")
			return
		}
		uid := userID(r.Context())
		var b captureReq
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		b.Text = strings.TrimSpace(b.Text)
		if b.Text == "" {
			errJSON(w, 400, "missing text")
			return
		}
		if len(b.Text) > 2000 {
			b.Text = b.Text[:2000]
		}
		if b.Source != "notification" {
			b.Source = "sms"
		}
		// A mail app carries many reporters: the phone sends its channel as
		// "package/from/subject" with digits dropped (Kotak's two NEFT emails,
		// Uber's receipt and Amazon Pay's debit are different reporters).
		channel := "sms"
		if b.Source == "notification" {
			channel = strings.ToLower(strings.TrimSpace(b.Sender))
			if c := strings.ToLower(strings.TrimSpace(b.Channel)); c != "" {
				channel = c
			}
		}
		if b.SenderLabel == "" {
			b.SenderLabel = map[bool]string{true: "bank SMS", false: "app notification"}[b.Source == "sms"]
		}
		ctx := r.Context()
		hash := captureHash(b.Text)
		ref := normRef(b.Ref)
		receivedAt := time.Now()
		if t, err := time.Parse(time.RFC3339, b.ReceivedAt); err == nil {
			receivedAt = t
		}
		record := func(c captureResp, amount float64, typ, desc string, acct *int64) {
			if _, err := d.ExecContext(ctx, `INSERT INTO fin_captures
				(user_id, source, channel, sender_label, text, text_hash, received_at, ref, amount, type, description, account_id, outcome, reason, txn_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,0))`,
				uid, b.Source, channel, b.SenderLabel, b.Text, hash, receivedAt, ref, amount, typ, desc, acct, c.Outcome, c.Reason, c.TxnID); err != nil {
				log.Warn().Err(err).Msg("record capture")
			}
		}

		// 1–2 before any AI call: the same text, or the same reference with
		// its amount in the text.
		if !b.Force {
			var txnID sql.NullInt64
			var amt float64
			var desc string
			if d.QueryRowContext(ctx, `SELECT txn_id, amount, description FROM fin_captures
				WHERE user_id = $1 AND text_hash = $2 AND outcome IN ('created','absorbed','confirmed') AND created_at > NOW() - $3::interval
				ORDER BY id DESC LIMIT 1`, uid, hash, fmt.Sprintf("%d seconds", int(captureRefWindow.Seconds()))).Scan(&txnID, &amt, &desc) == nil {
				c := captureResp{Outcome: "absorbed", Reason: "The same message again.", TxnID: txnID.Int64, Label: moneyLabel(amt, desc)}
				record(c, amt, "", desc, nil)
				writeJSON(w, 200, c)
				return
			}
			if ref != "" {
				if d.QueryRowContext(ctx, `SELECT txn_id, amount, description FROM fin_captures
					WHERE user_id = $1 AND ref = $2 AND outcome IN ('created','absorbed','confirmed') AND created_at > NOW() - $3::interval
					ORDER BY id DESC LIMIT 1`, uid, ref, fmt.Sprintf("%d seconds", int(captureRefWindow.Seconds()))).Scan(&txnID, &amt, &desc) == nil &&
					textMentionsAmount(b.Text, amt) {
					c := captureResp{Outcome: "absorbed", Reason: "Same reference and amount as a payment already added.", TxnID: txnID.Int64, Label: moneyLabel(amt, desc)}
					record(c, amt, "", desc, nil)
					writeJSON(w, 200, c)
					return
				}
			}
		}

		if ok, retryAfter := deps.AILimiter.check(uid); !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(math.Max(1, math.Ceil(retryAfter.Seconds())))))
			errJSON(w, 429, "AI hourly limit reached. Try again later.")
			return
		}
		loc := userLocation(d, uid)
		now := userNow(d, uid)
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		parsed, tokens, err := captureParser(pctx, deps, b.Text, now.Format("2006-01-02"))
		cancel()
		if tokens <= 0 {
			tokens = 80
		}
		deps.AILimiter.record(uid, tokens)
		if err != nil {
			log.Warn().Err(err).Msg("capture parse failed")
			record(captureResp{Outcome: "failed", Reason: "Couldn't read it."}, 0, "", "", nil)
			errJSON(w, http.StatusServiceUnavailable, "Couldn't read the message right now.")
			return
		}
		if parsed.Amount <= 0 {
			c := captureResp{Outcome: "ignored", Reason: "Not money that has moved."}
			record(c, 0, "", "", nil)
			writeJSON(w, 200, c)
			return
		}
		if ref == "" {
			ref = normRef(parsed.RefID)
		}
		cctx, ccancel := context.WithTimeout(ctx, 6*time.Second)
		fields := resolveParsedTransaction(cctx, deps, uid, parsed, loc, now, true)
		ccancel()
		txnAt := composeTxnAt(loc, parsed.Date, parsed.Time, now)
		// No time in the message and it waited on the phone: the phone's
		// receipt time is the honest one.
		if parsed.Time == "" && now.Sub(receivedAt) > 3*time.Minute && receivedAt.Before(now) {
			txnAt = receivedAt.In(loc)
			fields["txn_at"] = txnAt.Format(time.RFC3339)
		}
		acct, _ := fields["account_id"].(*int64)
		catID, _ := fields["category_id"].(*int64)
		typ := parsed.Type
		resp := captureResp{Parsed: fields}

		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin capture", err)
			return
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('fin_txn_create:' || $1::text, 0))`, uid); err != nil {
			internalError(w, r, "lock capture", err)
			return
		}
		finish := func(c captureResp) {
			c.Parsed = fields
			if _, err := tx.ExecContext(ctx, `INSERT INTO fin_captures
				(user_id, source, channel, sender_label, text, text_hash, received_at, ref, amount, type, description, account_id, outcome, reason, txn_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,0))`,
				uid, b.Source, channel, b.SenderLabel, b.Text, hash, receivedAt, ref, parsed.Amount, typ, parsed.Description, acct, c.Outcome, c.Reason, c.TxnID); err != nil {
				internalError(w, r, "record capture", err)
				return
			}
			if err := tx.Commit(); err != nil {
				internalError(w, r, "commit capture", err)
				return
			}
			code := 200
			if c.Outcome == "created" {
				code = 201
			}
			writeJSON(w, code, c)
		}

		if !b.Force && ref != "" {
			var txnID sql.NullInt64
			var amt float64
			var desc string
			if tx.QueryRowContext(ctx, `SELECT txn_id, amount, description FROM fin_captures
				WHERE user_id = $1 AND ref = $2 AND outcome IN ('created','absorbed','confirmed') AND created_at > NOW() - $3::interval
				ORDER BY id DESC LIMIT 1`, uid, ref, fmt.Sprintf("%d seconds", int(captureRefWindow.Seconds()))).Scan(&txnID, &amt, &desc) == nil {
				if math.Abs(amt-parsed.Amount) < 0.005 {
					resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "absorbed", "Same reference and amount as a payment already added.", txnID.Int64, moneyLabel(amt, desc)
				} else {
					resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "asked", "Same reference as an earlier payment, but a different amount.", txnID.Int64, moneyLabel(amt, desc)
				}
				finish(resp)
				return
			}
		}

		if acct != nil && !b.Force {
			// The bank confirming an instalment Sajni posted (RD, SIP).
			if typ == "expense" {
				var invTxn int64
				var invName string
				if tx.QueryRowContext(ctx, `
					SELECT t.id, i.name FROM fin_transactions t JOIN fin_investments i ON i.id = t.investment_id
					WHERE t.user_id = $1 AND t.account_id = $2 AND t.type = 'transfer_out' AND ABS(t.amount - $3) < 0.001
					  AND ABS(EXTRACT(EPOCH FROM (t.txn_at - $4))) < 36*3600
					ORDER BY ABS(EXTRACT(EPOCH FROM (t.txn_at - $4))) LIMIT 1`, uid, *acct, parsed.Amount, txnAt).Scan(&invTxn, &invName) == nil {
					resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "confirmed", "The bank confirming "+invName+", already recorded.", invTxn, moneyLabel(parsed.Amount, invName)
					finish(resp)
					return
				}
			}
			// An investment maturing into this account.
			if typ == "income" {
				var invID int64
				var invName string
				if tx.QueryRowContext(ctx, `
					SELECT id, name FROM fin_investments
					WHERE user_id = $1 AND account_id = $2 AND maturity_date IS NOT NULL
					  AND invested_amount > 0 AND $3 >= invested_amount - 0.5
					  AND ABS(EXTRACT(EPOCH FROM (maturity_date - $4::timestamptz))) <= 2*86400 + 43200
					ORDER BY ABS(EXTRACT(EPOCH FROM (maturity_date - $4::timestamptz))) LIMIT 1`, uid, *acct, parsed.Amount, txnAt).Scan(&invID, &invName) == nil {
					id, gain, err := db.TransferFromInvestment(ctx, tx, uid, *acct, invID, parsed.Amount, invName+" matured", parsed.Note, txnAt, 0)
					if err != nil {
						internalError(w, r, "file maturity", err)
						return
					}
					var interest float64
					if gain > 0 {
						tx.QueryRowContext(ctx, `SELECT amount FROM fin_transactions WHERE id = $1`, gain).Scan(&interest)
					}
					resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "created", invName+" matured.", id, moneyLabel(parsed.Amount, invName+" matured")
					resp.Matured, resp.Interest = invName, interest
					finish(resp)
					return
				}
			}
		}

		// 4. One report per channel per payment.
		if !b.Force {
			if id, label, ok, err := copyFromOtherChannel(ctx, tx, uid, channel, ref, typ, parsed.Amount, acct, txnAt); err != nil {
				internalError(w, r, "match capture copy", err)
				return
			} else if ok {
				resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "absorbed", "Another copy of a payment already added.", id, label
				finish(resp)
				return
			}
		}

		if acct == nil {
			resp.Outcome, resp.Reason, resp.Label = "review", "No account matched.", moneyLabel(parsed.Amount, parsed.Description)
			finish(resp)
			return
		}
		desc := strings.TrimSpace(parsed.Description)
		if desc == "" {
			desc = fallbackTitle(ctx, d, uid, catID, typ)
		}
		slateID, _ := resolveSlateID(d, uid, nil)
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, note, txn_at, slate_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
			uid, *acct, catID, typ, parsed.Amount, desc, parsed.Note, txnAt, slateID).Scan(&id); err != nil {
			internalError(w, r, "insert captured transaction", err)
			return
		}
		resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "created", "", id, moneyLabel(parsed.Amount, desc)
		if b.Force {
			resp.Reason = "Added anyway."
		}
		finish(resp)
	}
}

// copyFromOtherChannel finds the row this message is another channel's copy
// of: same type and amount (and account, when known) within
// captureCopyWindow, not yet reported by this channel, and not carrying a
// different reference. The closest in time wins.
func copyFromOtherChannel(ctx context.Context, tx *sql.Tx, uid, channel, ref, typ string, amount float64, acct *int64, at time.Time) (int64, string, bool, error) {
	types := []string{"expense", "lend", "transfer_out", "borrow_repayment"}
	if typ == "income" {
		types = []string{"income", "lend_repayment", "refund", "transfer_in", "borrow"}
	}
	var acctArg any
	if acct != nil {
		acctArg = *acct
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.amount, t.description,
		       EXISTS (SELECT 1 FROM fin_captures c WHERE c.txn_id = t.id AND c.channel = $6),
		       COALESCE((SELECT string_agg(DISTINCT c.ref, ',') FROM fin_captures c WHERE c.txn_id = t.id AND c.ref <> ''), '')
		FROM fin_transactions t
		WHERE t.user_id = $1 AND t.type = ANY($2) AND ABS(t.amount - $3) < 0.005
		  AND ($4::bigint IS NULL OR t.account_id = $4)
		  AND ABS(EXTRACT(EPOCH FROM (t.txn_at - $5))) <= $7
		ORDER BY ABS(EXTRACT(EPOCH FROM (t.txn_at - $5)))`,
		uid, types, amount, acctArg, at, channel, captureCopyWindow.Seconds())
	if err != nil {
		return 0, "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var amt float64
		var desc, refs string
		var reported bool
		if err := rows.Scan(&id, &amt, &desc, &reported, &refs); err != nil {
			return 0, "", false, err
		}
		if reported {
			continue // this channel already told us about that one: a new payment
		}
		if ref != "" && refs != "" && !strings.Contains(","+refs+",", ","+ref+",") {
			continue // both carry references and they differ: two payments
		}
		return id, moneyLabel(amt, desc), true, nil
	}
	return 0, "", false, rows.Err()
}

// purgeCaptureText clears raw message text after 30 days and drops capture
// rows after a year (the parsed fields and outcome are what's kept).
func purgeCaptureText(ctx context.Context, d *db.DB) (int64, error) {
	res, err := d.ExecContext(ctx, `UPDATE fin_captures SET text = NULL WHERE text IS NOT NULL AND created_at < NOW() - INTERVAL '30 days'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := d.ExecContext(ctx, `DELETE FROM fin_captures WHERE created_at < NOW() - INTERVAL '365 days'`); err != nil {
		return n, err
	}
	return n, nil
}

// listCaptures returns the latest captures for the capture log.
func listCaptures(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := recentCaptures(r.Context(), deps.DB, userID(r.Context()), 100)
		if err != nil {
			internalError(w, r, "list captures", err)
			return
		}
		writeJSON(w, 200, out)
	}
}

type captureRow struct {
	ID          int64   `json:"id"`
	At          string  `json:"at"`
	Source      string  `json:"source"`
	SenderLabel string  `json:"sender_label"`
	Text        *string `json:"text"`
	Amount      float64 `json:"amount"`
	Type        string  `json:"type"`
	Description string  `json:"description"`
	Outcome     string  `json:"outcome"`
	Reason      string  `json:"reason"`
	TxnID       *int64  `json:"txn_id"`
}

func recentCaptures(ctx context.Context, d *db.DB, uid string, limit int) ([]captureRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, created_at, source, sender_label, text, amount, type, description, outcome, reason, txn_id
		FROM fin_captures WHERE user_id = $1 ORDER BY id DESC LIMIT $2`, uid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []captureRow{}
	for rows.Next() {
		var c captureRow
		var at time.Time
		if err := rows.Scan(&c.ID, &at, &c.Source, &c.SenderLabel, &c.Text, &c.Amount, &c.Type, &c.Description, &c.Outcome, &c.Reason, &c.TxnID); err != nil {
			return nil, err
		}
		c.At = at.Format(time.RFC3339)
		out = append(out, c)
	}
	return out, rows.Err()
}
