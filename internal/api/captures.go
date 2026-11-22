package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
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
// A copy keeps the best of both: a fuller name (the Kotak app cuts names at
// 14 letters, the SMS doesn't) or a shop's receipt naming what was bought
// replaces a title Sajni chose, never one the owner typed. A receipt never
// makes a row by itself; with no bank message to join it waits as a review,
// and a later message for the same payment resolves that review (the reply
// lists it in Resolved, so the phone clears its notification).
//
// A card bill paid from the bank is a transfer: the card's "payment
// received" pairs with the bank's debit, and when the card was credited a
// little more than the bank paid (POP's ₹18 off a ₹9,392.77 bill), the gap is
// Cashback income on the card.
//
// "Add anyway" re-posts with force and skips 1, 2 and 4.

const (
	captureRefWindow  = 14 * 24 * time.Hour
	captureCopyWindow = 3 * time.Hour
	// The same text with no reference, not ignored, is one message only
	// this long: the Kotak app words every ₹22 to Salman identically.
	captureSameTextWindow = 30 * time.Minute
	// A card's "payment received" can trail the bank's debit by hours.
	cardBillWindow = 24 * time.Hour
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
	// Transfer names both sides when the message was filed as money moving
	// between the owner's accounts ("ICICI Savings → Amazon Wallet").
	Transfer string `json:"transfer,omitempty"`
	// Suggest is a one-tap link offered with a new row: a credit from
	// someone who owes the owner → "Paid back · Papa".
	Suggest *captureSuggest `json:"suggest,omitempty"`
	// CaptureID is this message's capture: the phone numbers a review
	// notification by it, and filing the review sends it back.
	CaptureID int64 `json:"capture_id,omitempty"`
	// Resolved are earlier reviews this message settled (the same payment,
	// now with its account): the phone clears their notifications.
	Resolved []int64 `json:"resolved,omitempty"`
	// Recurring is the recurring item this payment ticked ("Home Contrib"),
	// RecurringDue the cycle it paid ("2026-12-05").
	Recurring    string `json:"recurring,omitempty"`
	RecurringDue string `json:"recurring_due,omitempty"`
}

type captureSuggest struct {
	Kind   string  `json:"kind"`   // settle
	Person string  `json:"person"` // as on their lends
	Owes   float64 `json:"owes"`   // their total before this
}

var (
	captureSpaces = regexp.MustCompile(`\s+`)
	// An SMS app's notification starts with the sender ID ("JD-ZOMATO-S
	// Payment of Rs. 6.38…"); the SMS itself doesn't.
	smsSenderPrefix = regexp.MustCompile(`^[A-Z]{2}-[A-Z0-9]{3,12}(-[A-Z])?\s+`)
	// A clock with its half of the day ("11:40 PM", "11.40pm").
	reAmPm = regexp.MustCompile(`(?i)\d\s*(am|pm|a\.m\.|p\.m\.)(\W|$)`)
	// An FD (or "fixed deposit") maturing.
	reFDMatured = regexp.MustCompile(`(?i)\b(fd|fixed deposit|term deposit)\b.{0,40}\bmatur|\bmatur\w*\b.{0,40}\b(fd|fixed deposit|term deposit)\b`)
	// A card saying its bill was paid ("Payment of Rs 9,392.77 received",
	// "Thank you for your payment").
	reCardPaid = regexp.MustCompile(`(?i)\b(re)?payment\b.{0,60}\b(received|credited|successful|processed|realised)\b|\breceived\b.{0,40}\bpayment\b|\bthank you for\b.{0,30}\bpayment\b`)
)

func captureHash(text string) string {
	norm := captureSpaces.ReplaceAllString(strings.ToLower(smsSenderPrefix.ReplaceAllString(strings.TrimSpace(text), "")), " ")
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
var captureParser = func(ctx context.Context, deps Deps, uid, text, today string) (ai.ParsedTxn, int, error) {
	return deps.AI.ParseTransactionMessage(ctx, text, today, true, txnCategories(ctx, deps.DB, uid))
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
			var was string
			// Asked about or ignored before counts too: the Messages app
			// re-posting an SMS shouldn't ask a second time.
			// Only an ignored message or one with a reference is the same for
			// 14 days; the rest for half an hour, since a notification without
			// date or reference reads the same every time.
			if d.QueryRowContext(ctx, `SELECT txn_id, amount, description, outcome FROM fin_captures
				WHERE user_id = $1 AND text_hash = $2 AND outcome IN ('created','absorbed','confirmed','review','ignored')
				  AND created_at > NOW() - CASE WHEN outcome = 'ignored' OR ref <> '' THEN $3::interval ELSE $4::interval END
				ORDER BY id DESC LIMIT 1`, uid, hash, fmt.Sprintf("%d seconds", int(captureRefWindow.Seconds())),
				fmt.Sprintf("%d seconds", int(captureSameTextWindow.Seconds()))).Scan(&txnID, &amt, &desc, &was) == nil {
				c := captureResp{Outcome: "absorbed", Reason: "The same message again.", TxnID: txnID.Int64, Label: moneyLabel(amt, desc)}
				if was == "ignored" {
					c = captureResp{Outcome: "ignored", Reason: "The same message again."}
				}
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

		if ok, retryAfter := deps.AILimiter.checkCheap(r.Context(), uid); !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(math.Max(1, math.Ceil(retryAfter.Seconds())))))
			errJSON(w, 429, aiLimitMessage(retryAfter))
			return
		}
		loc := userLocation(d, uid)
		now := userNow(d, uid)
		pctx, cancel := context.WithTimeout(ctx, 9*time.Second) // under the phone's 10 s read timeout
		parsed, _, err := captureParser(pctx, deps, uid, b.Text, now.Format("2006-01-02"))
		cancel()
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
		fields := resolveParsedTransaction(cctx, deps, uid, parsed, b.Text, loc, now, true)
		ccancel()
		txnAt := composeTxnAt(loc, parsed.Date, parsed.Time, now)
		// No time in the message and it waited on the phone: the phone's
		// receipt time is the honest one.
		if parsed.Time == "" && now.Sub(receivedAt) > 3*time.Minute && receivedAt.Before(now) {
			txnAt = receivedAt.In(loc)
			fields["txn_at"] = txnAt.Format(time.RFC3339)
		}
		// A 12-hour clock without AM/PM (ICICI's card email: "at 11:40:02"
		// for 23:40) read as morning: the message can't predate its own
		// arrival by half a day, so take the evening.
		if parsed.Time != "" && !reAmPm.MatchString(b.Text) && txnAt.Hour() < 12 {
			if gap := receivedAt.Sub(txnAt); gap > 11*time.Hour && gap < 13*time.Hour {
				txnAt = txnAt.Add(12 * time.Hour)
				fields["txn_at"] = txnAt.Format(time.RFC3339)
			}
		}
		own := parsed.OwnMoney || ownName(ctx, d, uid, parsed.Description)
		hint := strings.ToLower(strings.TrimSpace(parsed.AccountHint))
		acct, _ := fields["account_id"].(*int64)
		typ := parsed.Type
		receipt := parsed.Receipt
		if receipt {
			// "UPI: Amazon" names how a receipt was paid far too loosely to
			// pick an account (it put a wallet payment on Salary).
			acct = nil
			fields["account_id"] = acct
		}
		card := acct != nil && accountType(ctx, d, uid, *acct) == "credit_card"
		if card && typ == "income" && reCardPaid.MatchString(b.Text) {
			own = true // the card's bill, paid from one of the owner's accounts
		}
		catID, _ := fields["category_id"].(*int64)
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
			if err := tx.QueryRowContext(ctx, `INSERT INTO fin_captures
				(user_id, source, channel, sender_label, text, text_hash, received_at, ref, amount, type, description, account_id, outcome, reason, txn_id, suggestion, own_money, account_hint, receipt)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,0),$16,$17,$18,$19) RETURNING id`,
				uid, b.Source, channel, b.SenderLabel, b.Text, hash, receivedAt, ref, parsed.Amount, typ, parsed.Description, acct, c.Outcome, c.Reason, c.TxnID, suggestionKey(c.Suggest), own, hint, receipt).Scan(&c.CaptureID); err != nil {
				internalError(w, r, "record capture", err)
				return
			}
			// The same payment asked about earlier, now known: settle that
			// review, and take a receipt's shop name.
			if (c.Outcome == "created" || c.Outcome == "absorbed") && c.TxnID != 0 {
				ids, err := resolveReviews(ctx, tx, uid, c.CaptureID, c.TxnID, channel, ref, typ, parsed.Amount, receivedAt)
				if err != nil {
					internalError(w, r, "resolve reviews", err)
					return
				}
				c.Resolved = ids
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

		// A broker or wallet confirming money from the owner's bank names both
		// accounts ("credited to your Kotak Neo account … Account number
		// XXXXXX2851"), and the hint can land on the bank, which already holds
		// this payment's other side. Money can't leave an account and come
		// back to it as the owner's own: this side belongs to the other
		// account the message names.
		if own && acct != nil && !b.Force && !receipt {
			if other := otherNamedAccount(ctx, tx, uid, b.Text, *acct); other != 0 {
				if _, _, ok, err := ownMoneySide(ctx, tx, uid, channel, typ, parsed.Amount, *acct, txnAt, receivedAt, own, true); err != nil {
					internalError(w, r, "own money side", err)
					return
				} else if ok {
					acct = &other
					fields["account_id"] = acct
					card = accountType(ctx, tx, uid, other) == "credit_card"
				}
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
					  AND EXTRACT(EPOCH FROM ($4::timestamptz - maturity_date)) BETWEEN -1.5*86400 AND 7.5*86400
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

		// An FD Sajni doesn't track maturing (Kotak's ActivMoney sweeps, in
		// multiples of ₹5,000): the round part is the owner's own money
		// coming back, only the rest is interest. ₹20,493 → ₹493 interest.
		if acct != nil && typ == "income" && reFDMatured.MatchString(b.Text) {
			if !b.Force {
				var prior int64
				var priorAmt float64
				var priorDesc string
				if tx.QueryRowContext(ctx, `SELECT c.txn_id, t.amount, t.description FROM fin_captures c JOIN fin_transactions t ON t.id = c.txn_id
					WHERE c.user_id = $1 AND c.type = 'income' AND ABS(c.amount - $2) < 0.005 AND c.outcome IN ('created','absorbed')
					  AND c.channel <> $3 AND ABS(EXTRACT(EPOCH FROM (c.received_at - $4))) <= $5
					ORDER BY c.id DESC LIMIT 1`, uid, parsed.Amount, channel, receivedAt, captureCopyWindow.Seconds()).Scan(&prior, &priorAmt, &priorDesc) == nil {
					resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "absorbed", "Another copy of a payment already added.", prior, moneyLabel(priorAmt, priorDesc)
					finish(resp)
					return
				}
			}
			principal := math.Floor(parsed.Amount/5000) * 5000
			interest := math.Round((parsed.Amount-principal)*100) / 100
			if interest < 0.01 {
				resp.Outcome, resp.Reason = "ignored", "An FD maturing: your own money coming back."
				finish(resp)
				return
			}
			var interestCat sql.NullInt64
			tx.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND kind = 'income' AND LOWER(name) = 'interest' LIMIT 1`, uid).Scan(&interestCat)
			if interestCat.Valid {
				catID = &interestCat.Int64
			}
			slateID, _ := resolveSlateID(d, uid, nil)
			note := fmt.Sprintf("FD matured ₹%s; ₹%s of it was your own money", formatINR(parsed.Amount), formatINR(principal))
			var id int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, note, txn_at, slate_id)
				VALUES ($1,$2,$3,'income',$4,'FD interest',$5,$6,$7) RETURNING id`,
				uid, *acct, catID, interest, note, txnAt, slateID).Scan(&id); err != nil {
				internalError(w, r, "insert fd interest", err)
				return
			}
			resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "created", "Only the interest is income.", id, moneyLabel(interest, "FD interest")
			resp.Matured, resp.Interest = "FD", interest
			finish(resp)
			return
		}

		// 4. One report per channel per payment.
		if !b.Force {
			// A receipt's number is the shop's order id, not the bank's
			// reference: it can't tell two payments apart.
			copyRef := ref
			if receipt {
				copyRef = ""
			}
			if id, label, ok, err := copyFromOtherChannel(ctx, tx, uid, channel, copyRef, typ, parsed.Amount, acct, txnAt, receivedAt); err != nil {
				internalError(w, r, "match capture copy", err)
				return
			} else if ok {
				if title, err := betterTitle(ctx, tx, uid, id, parsed.Description, receipt); err != nil {
					internalError(w, r, "copy title", err)
					return
				} else if title != "" {
					label = moneyLabel(parsed.Amount, title)
				}
				resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "absorbed", "Another copy of a payment already added.", id, label
				finish(resp)
				return
			}
		}

		// A receipt with no bank message yet: ask, and let the bank's
		// message settle it when it comes.
		if receipt && !b.Force {
			resp.Outcome, resp.Reason, resp.Label = "review", "A receipt; the bank's message for it hasn't come.", moneyLabel(parsed.Amount, parsed.Description)
			finish(resp)
			return
		}

		if acct == nil {
			// An account the owner said not to track (Zomato Money).
			var label string
			if hint != "" && tx.QueryRowContext(ctx, `SELECT label FROM fin_capture_ignores WHERE user_id = $1 AND hint = $2`, uid, hint).Scan(&label) == nil {
				resp.Outcome, resp.Reason = "ignored", "You don't track "+label+"."
				finish(resp)
				return
			}
			resp.Outcome, resp.Reason, resp.Label = "review", "No account matched.", moneyLabel(parsed.Amount, parsed.Description)
			finish(resp)
			return
		}

		// The other side of money moving between the owner's own accounts:
		// ICICI's "debited; Amazon Pay Bala credited" SMS, then Amazon's
		// "₹500 added to your Amazon Pay balance" email → one transfer.
		if !b.Force {
			if id, name, ok, err := pairOwnMoney(ctx, tx, uid, channel, typ, parsed.Amount, *acct, txnAt, receivedAt, own); err != nil {
				internalError(w, r, "pair transfer", err)
				return
			} else if ok {
				newLeg, err := db.MakeTransfer(ctx, tx, uid, id, *acct)
				var conflict *db.TxnConflict
				if err != nil && !errors.As(err, &conflict) {
					internalError(w, r, "pair transfer", err)
					return
				}
				if err == nil {
					from, to := name, accountName(ctx, tx, uid, *acct)
					if typ == "expense" {
						from, to = to, name
					}
					title := "Transfer to " + to
					tx.ExecContext(ctx, `UPDATE fin_transactions SET description = $1 WHERE id IN ($2,$3) AND user_id = $4`, title, id, newLeg, uid)
					resp.Outcome, resp.Reason, resp.TxnID, resp.Label = "created", "The other side of a transfer.", newLeg, moneyLabel(parsed.Amount, title)
					resp.Transfer = from + " → " + to
					finish(resp)
					return
				}
			}
			// A card bill: the bank's debit and the card's "payment received",
			// a discount apart.
			if c, ok, err := pairCardBill(ctx, tx, uid, channel, typ, parsed.Amount, *acct, card, own, txnAt); err != nil {
				internalError(w, r, "pair card bill", err)
				return
			} else if ok {
				finish(c)
				return
			}
		}
		desc := strings.TrimSpace(parsed.Description)
		// The owner's own name for this payee ("Bhola Khan" → "Sewri";
		// Salman by amount: Office Smoke or Salman's shop).
		if title, cat, ok := lookupPayeeName(ctx, tx, uid, desc, typ, parsed.Amount); ok {
			desc = title
			if cat != nil {
				catID = cat
			}
		}
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
		// A payee the owner turned into a transfer before (their own name
		// on a Kotak debit = loading UPI Lite).
		if other, ok := lookupPayeeTransfer(ctx, tx, uid, parsed.Description, typ); ok && other != *acct {
			newLeg, err := db.MakeTransfer(ctx, tx, uid, id, other)
			if err != nil {
				internalError(w, r, "learned transfer", err)
				return
			}
			from, to := accountName(ctx, tx, uid, *acct), accountName(ctx, tx, uid, other)
			if typ == "income" {
				from, to = to, from
			}
			title := "Transfer to " + to
			tx.ExecContext(ctx, `UPDATE fin_transactions SET description = $1 WHERE id IN ($2,$3) AND user_id = $4`, title, id, newLeg, uid)
			resp.Label, resp.Transfer = moneyLabel(parsed.Amount, title), from+" → "+to
			finish(resp)
			return
		}
		// A payment that fits a recurring item ticks it and takes its name
		// (₹22,000 to Papa on the 5th → Home Contrib).
		if m, ok, err := db.MatchRecurring(ctx, tx, uid, id, true); err != nil {
			internalError(w, r, "match recurring", err)
			return
		} else if ok {
			resp.Recurring, resp.RecurringDue, resp.Label = m.Name, m.Due, moneyLabel(parsed.Amount, m.Title)
			finish(resp)
			return
		}
		if typ == "income" {
			resp.Suggest = suggestSettle(ctx, tx, uid, parsed.Description)
		}
		finish(resp)
	}
}

func suggestionKey(s *captureSuggest) string {
	if s == nil {
		return ""
	}
	return s.Kind + ":" + s.Person
}

// suggestSettle offers "Paid back · <person>" for a credit from someone who
// owes the owner: the bank's name as learned from an earlier settle
// (fin_payee_people), else a person whose name is the payee's name or its
// first word ("Alefiya" for "Alefiya Abbas Rampur"), when exactly one fits.
// An experiment: fin_captures records each offer and whether it was taken.
func suggestSettle(ctx context.Context, tx *sql.Tx, uid, payee string) *captureSuggest {
	p := strings.ToLower(strings.TrimSpace(payee))
	if p == "" {
		return nil
	}
	var alias string
	tx.QueryRowContext(ctx, `SELECT LOWER(BTRIM(person)) FROM fin_payee_people
		WHERE user_id = $1 AND (payee = $2 OR (length($2) >= $3 AND (starts_with(payee, $2)
		      OR (length(payee) >= $3 AND starts_with($2, payee)))))
		ORDER BY (payee = $2) DESC, length(payee) DESC LIMIT 1`, uid, p, merchantPrefixMin).Scan(&alias)
	rows, err := tx.QueryContext(ctx, `
		WITH owed AS (
			SELECT LOWER(BTRIM(borrower)) AS k, MAX(borrower) AS person, SUM(principal) AS owed
			FROM fin_lends WHERE user_id = $1 GROUP BY 1
		), paid AS (
			SELECT LOWER(BTRIM(s.borrower)) AS k, SUM(t.amount) AS paid
			FROM fin_lend_settlements s JOIN fin_transactions t ON t.id = s.transaction_id
			WHERE s.user_id = $1 GROUP BY 1
		)
		SELECT o.person, o.owed - COALESCE(p.paid, 0)
		FROM owed o LEFT JOIN paid p ON p.k = o.k
		WHERE o.owed - COALESCE(p.paid, 0) > 0.005
		  AND (CASE WHEN $2 <> '' THEN o.k = $2 ELSE o.k = $3 OR o.k = split_part($3, ' ', 1) END)
		LIMIT 2`, uid, alias, p)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []captureSuggest
	for rows.Next() {
		var s captureSuggest
		if rows.Scan(&s.Person, &s.Owes) == nil {
			s.Kind = "settle"
			out = append(out, s)
		}
	}
	if len(out) != 1 {
		return nil
	}
	return &out[0]
}

// copyFromOtherChannel finds the row this message is another channel's copy
// of: same type and amount (and account, when known) within
// captureCopyWindow, not yet reported by this channel, and not carrying a
// different reference. The closest in time wins.
func copyFromOtherChannel(ctx context.Context, tx *sql.Tx, uid, channel, ref, typ string, amount float64, acct *int64, at, received time.Time) (int64, string, bool, error) {
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
		  AND (ABS(EXTRACT(EPOCH FROM (t.txn_at - $5))) <= $7
		       OR EXISTS (SELECT 1 FROM fin_captures c WHERE c.txn_id = t.id AND ABS(EXTRACT(EPOCH FROM (c.received_at - $8))) <= $7))
		ORDER BY ABS(EXTRACT(EPOCH FROM (t.txn_at - $5)))`,
		uid, types, amount, acctArg, at, channel, captureCopyWindow.Seconds(), received)
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

// purgeAICalls drops AI usage ledger rows older than a year.
func purgeAICalls(ctx context.Context, d *db.DB) (int64, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM ai_calls WHERE at < NOW() - INTERVAL '1 year'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
	Suggestion  string  `json:"suggestion,omitempty"`
	Taken       bool    `json:"suggestion_taken,omitempty"`
}

func recentCaptures(ctx context.Context, d *db.DB, uid string, limit int) ([]captureRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, created_at, source, sender_label, text, amount, type, description, outcome, reason, txn_id, suggestion, suggestion_taken IS NOT NULL
		FROM fin_captures WHERE user_id = $1 ORDER BY id DESC LIMIT $2`, uid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []captureRow{}
	for rows.Next() {
		var c captureRow
		var at time.Time
		if err := rows.Scan(&c.ID, &at, &c.Source, &c.SenderLabel, &c.Text, &c.Amount, &c.Type, &c.Description, &c.Outcome, &c.Reason, &c.TxnID, &c.Suggestion, &c.Taken); err != nil {
			return nil, err
		}
		c.At = at.Format(time.RFC3339)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ownName reports whether a payee is the owner by name: its first and last
// words both appear in their account name or email ("SNEHIL RONAK SHAH" in
// snehilshah27@…). Banks print the owner's own name when they load UPI
// Lite or send money to the owner's other bank.
func ownName(ctx context.Context, d *db.DB, uid, payee string) bool {
	words := strings.Fields(strings.ToLower(payee))
	if len(words) < 2 {
		return false
	}
	first, last := words[0], words[len(words)-1]
	if len(first) < 3 || len(last) < 3 {
		return false
	}
	var name, email string
	if d.QueryRowContext(ctx, `SELECT LOWER(COALESCE(name,'')), LOWER(COALESCE(email,'')) FROM users WHERE id = $1`, uid).Scan(&name, &email) != nil {
		return false
	}
	local, _, _ := strings.Cut(email, "@")
	for _, s := range []string{strings.ReplaceAll(name, " ", ""), local} {
		if s != "" && strings.Contains(s, first) && strings.Contains(s, last) {
			return true
		}
	}
	return false
}

// pairOwnMoney finds the captured row on another of the owner's accounts
// that is this message's other side: the opposite direction, the same
// amount, within captureCopyWindow, reported by another channel, and with
// at least one of the two messages saying the money stayed the owner's.
// Returns that row and its account's name.
func pairOwnMoney(ctx context.Context, tx *sql.Tx, uid, channel, typ string, amount float64, acct int64, at, received time.Time, own bool) (int64, string, bool, error) {
	return ownMoneySide(ctx, tx, uid, channel, typ, amount, acct, at, received, own, false)
}

// otherNamedAccount returns the one account other than acct that text names
// by its name or a name hint (whole words, the longest wins; a tie between
// two accounts is no answer), or 0.
func otherNamedAccount(ctx context.Context, tx *sql.Tx, uid, text string, acct int64) int64 {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, COALESCE(match_hints,'') FROM fin_accounts WHERE user_id = $1 AND archived = false AND id <> $2`, uid, acct)
	if err != nil {
		return 0
	}
	defer rows.Close()
	s := strings.ToLower(text)
	var best int64
	bestTok, tie := "", false
	for rows.Next() {
		var id int64
		var name, hints string
		if rows.Scan(&id, &name, &hints) != nil {
			continue
		}
		for _, tok := range append(splitMatchHints(hints), strings.ToLower(strings.TrimSpace(name))) {
			if isAllDigits(tok) || len(tok) < 4 || len(tok) < len(bestTok) || !hasWord(s, tok) {
				continue
			}
			if len(tok) == len(bestTok) {
				tie = tie || id != best
				continue
			}
			best, bestTok, tie = id, tok, false
		}
	}
	if tie {
		return 0
	}
	return best
}

// ownMoneySide finds the other side of own money on another account, or
// with same, on acct itself (a sign the message's account is the wrong one).
func ownMoneySide(ctx context.Context, tx *sql.Tx, uid, channel, typ string, amount float64, acct int64, at, received time.Time, own, same bool) (int64, string, bool, error) {
	other := "income"
	if typ == "income" {
		other = "expense"
	}
	var id int64
	var name string
	err := tx.QueryRowContext(ctx, `
		SELECT t.id, a.name FROM fin_transactions t
		JOIN fin_accounts a ON a.id = t.account_id
		JOIN fin_captures c ON c.txn_id = t.id AND c.outcome = 'created'
		WHERE t.user_id = $1 AND t.type = $2 AND ABS(t.amount - $3) < 0.005 AND (t.account_id = $4) = $10
		  AND c.channel <> $5 AND ($8 OR c.own_money)
		  AND (ABS(EXTRACT(EPOCH FROM (t.txn_at - $6))) <= $9 OR ABS(EXTRACT(EPOCH FROM (c.received_at - $7))) <= $9)
		ORDER BY ABS(EXTRACT(EPOCH FROM (t.txn_at - $6))) LIMIT 1`,
		uid, other, amount, acct, channel, at, received, own, captureCopyWindow.Seconds(), same).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	return id, name, err == nil, err
}

func accountName(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, uid string, id int64) string {
	var name string
	q.QueryRowContext(ctx, `SELECT name FROM fin_accounts WHERE id = $1 AND user_id = $2`, id, uid).Scan(&name)
	return name
}

// lookupPayeeTransfer returns the account a payee's payments were turned
// into transfers with: once on two payments, or once when the message said
// it was the owner's own money. Names match like payee names (exact, or a
// prefix of at least merchantPrefixMin).
func lookupPayeeTransfer(ctx context.Context, tx *sql.Tx, uid, payee, kind string) (int64, bool) {
	p := strings.ToLower(strings.TrimSpace(payee))
	if p == "" {
		return 0, false
	}
	var acct int64
	err := tx.QueryRowContext(ctx, `
		SELECT account_id FROM fin_payee_transfers
		WHERE user_id = $1 AND kind = $3 AND (uses >= 2 OR own)
		  AND (payee = $2 OR (length($2) >= $4 AND (starts_with(payee, $2) OR (length(payee) >= $4 AND starts_with($2, payee)))))
		ORDER BY (payee = $2) DESC, updated_at DESC LIMIT 1`, uid, p, kind, merchantPrefixMin).Scan(&acct)
	return acct, err == nil
}

// Accounts the owner doesn't track ("Don't track" on a review prompt):
// a capture naming one is ignored instead of asked.
type captureIgnore struct {
	Hint  string `json:"hint"`
	Label string `json:"label"`
	// Resolved: reviews this settled (now ignored); the phone clears them.
	Resolved []int64 `json:"resolved,omitempty"`
}

func listCaptureIgnores(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := captureIgnores(r.Context(), deps.DB, userID(r.Context()))
		if err != nil {
			internalError(w, r, "list capture ignores", err)
			return
		}
		writeJSON(w, 200, out)
	}
}

func captureIgnores(ctx context.Context, d *db.DB, uid string) ([]captureIgnore, error) {
	rows, err := d.QueryContext(ctx, `SELECT hint, label FROM fin_capture_ignores WHERE user_id = $1 ORDER BY created_at`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []captureIgnore{}
	for rows.Next() {
		var c captureIgnore
		if err := rows.Scan(&c.Hint, &c.Label); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// addCaptureIgnoreRow stores hint (lowercased) with a display label.
func addCaptureIgnoreRow(ctx context.Context, d *db.DB, uid, hint, label string) (captureIgnore, error) {
	c := captureIgnore{Hint: strings.ToLower(strings.TrimSpace(hint)), Label: strings.TrimSpace(label)}
	if c.Label == "" {
		c.Label = strings.TrimSpace(hint)
	}
	if c.Hint == "" {
		return c, errors.New("hint is required")
	}
	var err error
	c.Resolved, err = db.IgnoreCaptureAccount(ctx, d, uid, c.Hint, c.Label)
	return c, err
}

func addCaptureIgnore(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b captureIgnore
		if err := readJSON(r, &b); err != nil || strings.TrimSpace(b.Hint) == "" {
			errJSON(w, 400, "hint is required")
			return
		}
		c, err := addCaptureIgnoreRow(r.Context(), deps.DB, userID(r.Context()), b.Hint, b.Label)
		if err != nil {
			internalError(w, r, "add capture ignore", err)
			return
		}
		writeJSON(w, 200, c)
	}
}

func removeCaptureIgnore(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hint := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("hint")))
		if _, err := deps.DB.ExecContext(r.Context(), `DELETE FROM fin_capture_ignores WHERE user_id = $1 AND hint = $2`, userID(r.Context()), hint); err != nil {
			internalError(w, r, "remove capture ignore", err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func accountType(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, uid string, id int64) string {
	var typ string
	q.QueryRowContext(ctx, `SELECT type FROM fin_accounts WHERE id = $1 AND user_id = $2`, id, uid).Scan(&typ)
	return typ
}

// betterTitle lets a copy improve the title of the row it joins, only while
// that title is still the one capture chose (never one the owner typed or a
// learned payee name): a shop's receipt names what was bought ("Supercell"
// over the wallet's "Apay Wallet"), and a fuller name replaces one cut short
// ("Himanshu Jitendra Ch" over the Kotak app's "Himanshu Jiten"). Returns the
// new title, or "" when the row keeps its own.
func betterTitle(ctx context.Context, tx *sql.Tx, uid string, rowID int64, name string, receipt bool) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	var title, typ string
	var captured sql.NullString
	var untouched bool
	err := tx.QueryRowContext(ctx, `SELECT t.description, t.type,
			(SELECT c.description FROM fin_captures c WHERE c.txn_id = t.id AND c.outcome = 'created' ORDER BY c.id LIMIT 1),
			t.updated_at <= t.created_at + INTERVAL '2 seconds'
		FROM fin_transactions t WHERE t.id = $1 AND t.user_id = $2`, rowID, uid).Scan(&title, &typ, &captured, &untouched)
	if errors.Is(err, sql.ErrNoRows) || !captured.Valid {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if typ != "expense" && typ != "income" {
		return "", nil // a transfer's title names its accounts
	}
	title = strings.TrimSpace(title)
	capTitle := strings.TrimSpace(captured.String)
	auto := capTitle != "" && strings.EqualFold(title, capTitle) || capTitle == "" && untouched
	if !auto || strings.EqualFold(title, name) {
		return "", nil
	}
	cut := title != "" && len(name) > len(title) && strings.HasPrefix(strings.ToLower(name), strings.ToLower(title))
	// The first copy named no one ("₹8,850 debited" from the Kotak app), so
	// the row carries a stand-in like "Expense": any name is better.
	unnamed := capTitle == ""
	if !receipt && !cut && !unnamed {
		return "", nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET description = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, name, rowID, uid); err != nil {
		return "", err
	}
	return name, nil
}

// resolveReviews settles earlier reviews of the payment this capture just
// filed or joined: the same type and amount from another channel, with the
// same reference or within captureCopyWindow. A receipt among them lends the
// row its shop name. Returns their capture ids for the phone to clear.
func resolveReviews(ctx context.Context, tx *sql.Tx, uid string, captureID, txnID int64, channel, ref, typ string, amount float64, received time.Time) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `UPDATE fin_captures SET outcome = 'absorbed', txn_id = $2, reason = 'Settled by a later copy of the same payment.'
		WHERE user_id = $1 AND outcome = 'review' AND id <> $3 AND type = $4 AND ABS(amount - $5) < 0.005 AND channel <> $6
		  AND created_at > NOW() - $9::interval
		  AND (($7 <> '' AND ref = $7) OR ABS(EXTRACT(EPOCH FROM (received_at - $8))) <= $10)
		RETURNING id, receipt, description`,
		uid, txnID, captureID, typ, amount, channel, ref, received, fmt.Sprintf("%d seconds", int(captureRefWindow.Seconds())), captureCopyWindow.Seconds())
	if err != nil {
		return nil, err
	}
	var ids []int64
	shop := ""
	for rows.Next() {
		var id int64
		var receipt bool
		var desc string
		if err := rows.Scan(&id, &receipt, &desc); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		if receipt && shop == "" {
			shop = desc
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if shop != "" {
		if _, err := betterTitle(ctx, tx, uid, txnID, shop, true); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// pairCardBill files a card bill paid from the bank when the two sides
// differ by a discount: the card was credited a little more than the bank
// paid (POP's ₹18 off the ₹9,392.77 ICICI bill: Kotak paid ₹9,374.77). The
// bank's part is a transfer to the card and the gap Cashback income on it.
// Either message may come first; exact amounts are pairOwnMoney's.
func pairCardBill(ctx context.Context, tx *sql.Tx, uid, channel, typ string, amount float64, acct int64, card, own bool, at time.Time) (captureResp, bool, error) {
	var out captureResp
	// Both sides often come by SMS (Kotak's, then ICICI's), so unlike other
	// copies the channel may repeat: they are on two different accounts.
	const gapOK = `$3 - t.amount > -0.005 AND $3 - t.amount <= LEAST(500, 0.02 * $3)`
	win := cardBillWindow.Seconds()
	switch {
	case typ == "income" && card && own:
		// The bank's debit already a transfer to this card (a learned payee).
		var id int64
		var paid float64
		var title string
		err := tx.QueryRowContext(ctx, `SELECT t.id, t.amount, t.description FROM fin_transactions t
			WHERE t.user_id = $1 AND t.account_id = $2 AND t.type = 'transfer_in' AND `+gapOK+`
			  AND ABS(EXTRACT(EPOCH FROM (t.txn_at - $4))) <= $5
			  AND NOT EXISTS (SELECT 1 FROM fin_captures c WHERE c.txn_id = t.id AND c.channel = $6)
			ORDER BY ABS(EXTRACT(EPOCH FROM (t.txn_at - $4))) LIMIT 1`, uid, acct, amount, at, win, channel).Scan(&id, &paid, &title)
		if err == nil {
			if err := cardDiscount(ctx, tx, uid, acct, amount-paid, at); err != nil {
				return out, false, err
			}
			out.Outcome, out.Reason, out.TxnID, out.Label = "absorbed", "The card confirming its bill; the difference is cashback.", id, moneyLabel(paid, title)
			return out, true, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return out, false, err
		}
		// The bank's debit still an expense.
		var bank int64
		err = tx.QueryRowContext(ctx, `SELECT t.id, t.amount, t.account_id FROM fin_transactions t
			JOIN fin_captures c ON c.txn_id = t.id AND c.outcome = 'created'
			WHERE t.user_id = $1 AND t.account_id <> $2 AND t.type = 'expense' AND `+gapOK+`
			  AND ABS(EXTRACT(EPOCH FROM (t.txn_at - $4))) <= $5
			ORDER BY ABS(EXTRACT(EPOCH FROM (t.txn_at - $4))) LIMIT 1`, uid, acct, amount, at, win).Scan(&id, &paid, &bank)
		if errors.Is(err, sql.ErrNoRows) {
			return out, false, nil
		} else if err != nil {
			return out, false, err
		}
		newLeg, err := cardTransfer(ctx, tx, uid, id, acct, acct)
		if err != nil {
			return out, false, err
		}
		if err := cardDiscount(ctx, tx, uid, acct, amount-paid, at); err != nil {
			return out, false, err
		}
		out.Outcome, out.Reason, out.TxnID = "created", "The other side of a card bill; the difference is cashback.", newLeg
		out.Label = moneyLabel(paid, "Transfer to "+accountName(ctx, tx, uid, acct))
		out.Transfer = accountName(ctx, tx, uid, bank) + " → " + accountName(ctx, tx, uid, acct)
		return out, true, nil
	case typ == "expense" && !card:
		// The card's "payment received" already filed as income, a little
		// more than the bank paid: it becomes the bank's part, the rest
		// cashback.
		var id, cardAcct int64
		var credited float64
		err := tx.QueryRowContext(ctx, `SELECT t.id, t.amount, t.account_id FROM fin_transactions t
			JOIN fin_accounts a ON a.id = t.account_id AND a.type = 'credit_card'
			JOIN fin_captures c ON c.txn_id = t.id AND c.outcome = 'created'
			WHERE t.user_id = $1 AND t.account_id <> $2 AND t.type = 'income'
			  AND t.amount - $3 > -0.005 AND t.amount - $3 <= LEAST(500, 0.02 * t.amount)
			  AND (c.own_money OR $6) AND ABS(EXTRACT(EPOCH FROM (t.txn_at - $4))) <= $5
			ORDER BY ABS(EXTRACT(EPOCH FROM (t.txn_at - $4))) LIMIT 1`, uid, acct, amount, at, win, own).Scan(&id, &credited, &cardAcct)
		if errors.Is(err, sql.ErrNoRows) {
			return out, false, nil
		} else if err != nil {
			return out, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET amount = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3`, amount, id, uid); err != nil {
			return out, false, err
		}
		if err := cardDiscount(ctx, tx, uid, cardAcct, credited-amount, at); err != nil {
			return out, false, err
		}
		newLeg, err := cardTransfer(ctx, tx, uid, id, acct, cardAcct)
		if err != nil {
			return out, false, err
		}
		out.Outcome, out.Reason, out.TxnID = "created", "A card bill; the card got a little more, filed as cashback.", newLeg
		out.Label = moneyLabel(amount, "Transfer to "+accountName(ctx, tx, uid, cardAcct))
		out.Transfer = accountName(ctx, tx, uid, acct) + " → " + accountName(ctx, tx, uid, cardAcct)
		return out, true, nil
	}
	return out, false, nil
}

// cardTransfer turns row id into a transfer with account other (the new leg
// is on other) titled "Transfer to <card>".
func cardTransfer(ctx context.Context, tx *sql.Tx, uid string, id, other, to int64) (int64, error) {
	newLeg, err := db.MakeTransfer(ctx, tx, uid, id, other)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE fin_transactions SET description = $1 WHERE id IN ($2,$3) AND user_id = $4`,
		"Transfer to "+accountName(ctx, tx, uid, to), id, newLeg, uid)
	return newLeg, err
}

// cardDiscount files what reached the card beyond what the bank paid as
// Cashback income on the card.
func cardDiscount(ctx context.Context, tx *sql.Tx, uid string, card int64, gap float64, at time.Time) error {
	gap = math.Round(gap*100) / 100
	if gap < 0.01 {
		return nil
	}
	var cat sql.NullInt64
	tx.QueryRowContext(ctx, `SELECT id FROM fin_categories WHERE user_id = $1 AND kind = 'income' AND LOWER(BTRIM(name)) = 'cashback' LIMIT 1`, uid).Scan(&cat)
	var slate int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM fin_slates WHERE user_id = $1 AND is_plain`, uid).Scan(&slate); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at, slate_id)
		VALUES ($1,$2,$3,'income',$4,'Discount on card bill',$5,$6)`, uid, card, cat, gap, at, slate)
	return err
}
