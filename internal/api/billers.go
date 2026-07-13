package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
)

// Billers live alongside finance in two kinds: 'subscription' (fixed amount,
// may auto_renew — the cron posts the txn) and 'bill' (variable amount, e.g.
// electricity — amount is an optional estimate; the user marks paid with the
// actual). The hourly tick (see ProcessBillerCron) emits "upcoming" alerts
// before the due date and posts auto-renew txns once the date passes.

func registerBillerRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/finance/billers", listBillers(deps))
	mux.HandleFunc("POST /api/finance/billers", createBiller(deps))
	mux.HandleFunc("PUT /api/finance/billers/{id}", updateBiller(deps))
	mux.HandleFunc("DELETE /api/finance/billers/{id}", deleteBiller(deps))
	mux.HandleFunc("POST /api/finance/billers/{id}/pay", payBiller(deps))
	mux.HandleFunc("GET /api/finance/billers/{id}/payments", listBillerPayments(deps))

	mux.HandleFunc("GET /api/finance/billers/alerts", listBillerAlerts(deps))
	mux.HandleFunc("POST /api/finance/billers/alerts/{id}/seen", markBillerAlertSeen(deps))
}

// is_subscription/variable are legacy fields kept in the JSON until android
// parity ships; variable is derived from kind so old clients keep working.
type billerResp struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Kind           string   `json:"kind"`
	Amount         float64  `json:"amount"`
	Frequency      string   `json:"frequency"`
	NextDueDate    string   `json:"next_due_date"`
	AccountID      *int64   `json:"account_id"`
	AccountName    *string  `json:"account_name"`
	CategoryID     *int64   `json:"category_id"`
	CategoryName   *string  `json:"category_name"`
	CategoryColor  *string  `json:"category_color"`
	IsSubscription bool     `json:"is_subscription"`
	AutoRenew      bool     `json:"auto_renew"`
	RemindTask     bool     `json:"remind_task"`
	Variable       bool     `json:"variable"`
	AlertDays      int      `json:"alert_days"`
	Color          string   `json:"color"`
	Notes          string   `json:"notes"`
	Archived       bool     `json:"archived"`
	LastPaidDate   *string  `json:"last_paid_date"`
	LastPaidAmount *float64 `json:"last_paid_amount"`
	CreatedAt      string   `json:"created_at"`
}

func validBillerKind(k string) bool {
	return k == "subscription" || k == "bill"
}

// validFrequency keeps the schema strict (we lean on this in advanceDueDate).
func validFrequency(f string) bool {
	switch f {
	case "weekly", "fortnightly", "monthly", "bimonthly", "quarterly":
		return true
	}
	return false
}

// advanceDueDate rolls a due date forward by one period.
func advanceDueDate(d time.Time, freq string, anchorDay int) time.Time {
	switch freq {
	case "weekly":
		return d.AddDate(0, 0, 7)
	case "fortnightly":
		return d.AddDate(0, 0, 14)
	case "bimonthly":
		return advanceRecurringMonth(d, 2, anchorDay)
	case "quarterly":
		return advanceRecurringMonth(d, 3, anchorDay)
	default: // monthly
		return advanceRecurringMonth(d, 1, anchorDay)
	}
}

func listBillers(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		includeArchived := queryParam(r, "include_archived") == "true"

		q := `SELECT b.id, b.name, b.kind, b.amount, b.frequency, b.next_due_date::text,
			b.account_id, a.name, b.category_id, c.name, c.color,
			b.is_subscription, b.auto_renew, b.remind_task, b.alert_days, b.color, b.notes, b.archived,
			(SELECT MAX(paid_date)::text FROM fin_biller_payments p WHERE p.biller_id = b.id),
			(SELECT amount FROM fin_biller_payments p WHERE p.biller_id = b.id ORDER BY paid_date DESC, id DESC LIMIT 1),
			b.created_at::text
			FROM fin_billers b
			LEFT JOIN fin_accounts a ON a.id = b.account_id
			LEFT JOIN fin_categories c ON c.id = b.category_id
			WHERE b.user_id = $1`
		if !includeArchived {
			q += " AND b.archived = FALSE"
		}
		q += " ORDER BY b.next_due_date ASC, b.id ASC"

		rows, err := d.Query(q, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []billerResp{}
		for rows.Next() {
			var b billerResp
			rows.Scan(&b.ID, &b.Name, &b.Kind, &b.Amount, &b.Frequency, &b.NextDueDate,
				&b.AccountID, &b.AccountName, &b.CategoryID, &b.CategoryName, &b.CategoryColor,
				&b.IsSubscription, &b.AutoRenew, &b.RemindTask, &b.AlertDays, &b.Color, &b.Notes, &b.Archived,
				&b.LastPaidDate, &b.LastPaidAmount, &b.CreatedAt)
			b.Variable = b.Kind == "bill"
			out = append(out, b)
		}
		writeJSON(w, 200, out)
	}
}

func createBiller(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Name           string  `json:"name"`
			Kind           string  `json:"kind"`
			Amount         float64 `json:"amount"`
			Frequency      string  `json:"frequency"`
			NextDueDate    string  `json:"next_due_date"`
			AccountID      *int64  `json:"account_id"`
			CategoryID     *int64  `json:"category_id"`
			IsSubscription bool    `json:"is_subscription"`
			AutoRenew      bool    `json:"auto_renew"`
			RemindTask     bool    `json:"remind_task"`
			Variable       bool    `json:"variable"` // legacy clients: variable=true → kind=bill
			AlertDays      *int    `json:"alert_days"`
			Color          string  `json:"color"`
			Notes          string  `json:"notes"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			errJSON(w, 400, "name required")
			return
		}
		if body.Kind == "" {
			if body.Variable {
				body.Kind = "bill"
			} else {
				body.Kind = "subscription"
			}
		}
		if !validBillerKind(body.Kind) {
			errJSON(w, 400, "invalid kind")
			return
		}
		// Subscriptions have a fixed price; bills carry an optional estimate.
		if body.Kind == "subscription" && body.Amount <= 0 {
			errJSON(w, 400, "subscription needs a fixed amount")
			return
		}
		// Bills never auto-pay — the amount isn't known until the bill lands.
		if body.Kind == "bill" {
			body.AutoRenew = false
		}
		if body.Frequency == "" {
			body.Frequency = "monthly"
		}
		if !validFrequency(body.Frequency) {
			errJSON(w, 400, "invalid frequency")
			return
		}
		if body.NextDueDate == "" {
			body.NextDueDate = userNow(d, uid).Format("2006-01-02")
		}
		due, err := time.Parse("2006-01-02", body.NextDueDate)
		if err != nil {
			errJSON(w, 400, "invalid next_due_date")
			return
		}
		if body.Color == "" {
			body.Color = "#2D5A4F"
		}
		alertDays := 3
		if body.AlertDays != nil && *body.AlertDays >= 0 {
			alertDays = *body.AlertDays
		}
		if body.AutoRenew && body.AccountID == nil {
			errJSON(w, 400, "auto_renew requires account_id")
			return
		}
		for _, ref := range []struct {
			table string
			id    *int64
		}{{"fin_accounts", body.AccountID}, {"fin_categories", body.CategoryID}} {
			if ref.id != nil {
				if err := requireOwnedFinanceRef(r.Context(), d, ref.table, uid, *ref.id); err != nil {
					errJSON(w, 404, "not found")
					return
				}
			}
		}
		var id int64
		err = d.QueryRow(`INSERT INTO fin_billers
			(user_id, name, kind, amount, frequency, next_due_date, anchor_day, account_id, category_id,
			 is_subscription, auto_renew, remind_task, variable, alert_days, color, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
			uid, body.Name, body.Kind, body.Amount, body.Frequency, body.NextDueDate, due.Day(), body.AccountID, body.CategoryID,
			body.IsSubscription, body.AutoRenew, body.RemindTask, body.Kind == "bill", alertDays, body.Color, body.Notes).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateBiller(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Name           *string  `json:"name"`
			Kind           *string  `json:"kind"`
			Amount         *float64 `json:"amount"`
			Frequency      *string  `json:"frequency"`
			NextDueDate    *string  `json:"next_due_date"`
			AccountID      *int64   `json:"account_id"`
			CategoryID     *int64   `json:"category_id"`
			IsSubscription *bool    `json:"is_subscription"`
			AutoRenew      *bool    `json:"auto_renew"`
			RemindTask     *bool    `json:"remind_task"`
			Variable       *bool    `json:"variable"` // legacy alias for kind
			AlertDays      *int     `json:"alert_days"`
			Color          *string  `json:"color"`
			Notes          *string  `json:"notes"`
			Archived       *bool    `json:"archived"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Kind == nil && body.Variable != nil {
			k := "subscription"
			if *body.Variable {
				k = "bill"
			}
			body.Kind = &k
		}
		if body.Kind != nil && !validBillerKind(*body.Kind) {
			errJSON(w, 400, "invalid kind")
			return
		}
		for _, ref := range []struct {
			table string
			id    *int64
		}{{"fin_accounts", body.AccountID}, {"fin_categories", body.CategoryID}} {
			if ref.id != nil {
				if err := requireOwnedFinanceRef(r.Context(), d, ref.table, uid, *ref.id); err != nil {
					errJSON(w, 404, "not found")
					return
				}
			}
		}
		// Re-validate the kind rules against the row's effective state.
		var curKind string
		var curAmount float64
		if err := d.QueryRow(`SELECT kind, amount FROM fin_billers WHERE id = $1 AND user_id = $2`,
			id, uid).Scan(&curKind, &curAmount); err != nil {
			errJSON(w, 404, "biller not found")
			return
		}
		effKind, effAmount := curKind, curAmount
		if body.Kind != nil {
			effKind = *body.Kind
		}
		if body.Amount != nil {
			effAmount = *body.Amount
		}
		if effKind == "subscription" && effAmount <= 0 {
			errJSON(w, 400, "subscription needs a fixed amount")
			return
		}
		if effKind == "bill" {
			// Bills never auto-pay; force it off on any edit that lands in
			// (or stays in) bill kind.
			f := false
			body.AutoRenew = &f
		}
		set := []string{"updated_at = NOW()"}
		args := []any{}
		ph := 1
		add := func(col string, v any) {
			set = append(set, col+" = $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if body.Name != nil {
			add("name", *body.Name)
		}
		if body.Kind != nil {
			add("kind", *body.Kind)
			add("variable", *body.Kind == "bill")
		}
		if body.Amount != nil {
			add("amount", *body.Amount)
		}
		if body.Frequency != nil {
			if !validFrequency(*body.Frequency) {
				errJSON(w, 400, "invalid frequency")
				return
			}
			add("frequency", *body.Frequency)
		}
		if body.NextDueDate != nil {
			due, err := time.Parse("2006-01-02", *body.NextDueDate)
			if err != nil {
				errJSON(w, 400, "invalid next_due_date")
				return
			}
			add("next_due_date", *body.NextDueDate)
			add("anchor_day", due.Day())
		}
		if body.AccountID != nil {
			add("account_id", *body.AccountID)
		}
		if body.CategoryID != nil {
			add("category_id", *body.CategoryID)
		}
		if body.IsSubscription != nil {
			add("is_subscription", *body.IsSubscription)
		}
		if body.AutoRenew != nil {
			add("auto_renew", *body.AutoRenew)
		}
		if body.RemindTask != nil {
			add("remind_task", *body.RemindTask)
		}
		if body.AlertDays != nil {
			add("alert_days", *body.AlertDays)
		}
		if body.Color != nil {
			add("color", *body.Color)
		}
		if body.Notes != nil {
			add("notes", *body.Notes)
		}
		if body.Archived != nil {
			add("archived", *body.Archived)
		}
		args = append(args, id, uid)
		q := "UPDATE fin_billers SET " + strings.Join(set, ", ") +
			" WHERE id = $" + itoa(ph) + " AND user_id = $" + itoa(ph+1)
		if _, err := d.Exec(q, args...); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteBiller(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM fin_billers WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// payBiller records the biller's current due cycle and rolls next_due_date
// forward. Two modes:
//   - record (default): posts an expense txn like before; `amount` overrides
//     the stored amount (a Bill's mark-paid asks the actual).
//   - attach (attach_txn_ids set): links pre-existing txns to the cycle and
//     creates NO txn — no account needed since nothing posts.
//
// Idempotent on (biller_id, due_date); a second pay of the same cycle
// returns already_paid instead of double-posting.
func payBiller(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			PaidDate     string   `json:"paid_date"`
			Amount       *float64 `json:"amount"`
			AttachTxnIDs []int64  `json:"attach_txn_ids"`
		}
		readJSON(r, &body)

		var name, freq string
		var amount float64
		var dueDate string
		var anchorDay int
		var accountID, categoryID sql.NullInt64
		err = d.QueryRow(`SELECT name, amount, frequency, next_due_date::text, COALESCE(anchor_day, EXTRACT(DAY FROM next_due_date)::INTEGER), account_id, category_id
			FROM fin_billers WHERE id = $1 AND user_id = $2`,
			id, uid).Scan(&name, &amount, &freq, &dueDate, &anchorDay, &accountID, &categoryID)
		if err != nil {
			errJSON(w, 404, "biller not found")
			return
		}
		if body.Amount != nil {
			amount = *body.Amount
		}
		paidDate := body.PaidDate
		if paidDate == "" {
			paidDate = userNow(d, uid).Format("2006-01-02")
		}

		var txnID int64
		var alreadyPaid bool
		if len(body.AttachTxnIDs) > 0 {
			alreadyPaid, err = attachBillerTxns(r.Context(), deps, uid, id, body.AttachTxnIDs,
				body.Amount, paidDate, dueDate)
			if err != nil {
				errJSON(w, 400, err.Error())
				return
			}
		} else {
			if !accountID.Valid {
				errJSON(w, 400, "biller has no account; assign one before paying")
				return
			}
			txnID, alreadyPaid, err = postBillerTxn(r.Context(), deps, uid, id, accountID.Int64, categoryID,
				name, amount, paidDate, dueDate, false)
			if err != nil {
				errJSON(w, 500, err.Error())
				return
			}
		}

		due, _ := time.Parse("2006-01-02", dueDate)
		next := advanceDueDate(due, freq, anchorDay)
		d.Exec(`UPDATE fin_billers SET next_due_date = $1, updated_at = NOW(), last_run_at = NOW()
			WHERE id = $2 AND user_id = $3`, next.Format("2006-01-02"), id, uid)

		writeJSON(w, 200, map[string]any{
			"status":        "ok",
			"txn_id":        txnID,
			"already_paid":  alreadyPaid,
			"next_due_date": next.Format("2006-01-02"),
		})
	}
}

// postBillerTxn posts one biller cycle inside a single DB transaction,
// payment-row-FIRST so the UNIQUE(biller_id, due_date) key gates the txn
// insert — concurrent manual pay + cron can never double-post a cycle.
// Returns (0, true, nil) when the cycle was already recorded.
func postBillerTxn(ctx context.Context, deps Deps, uid string, billerID, accountID int64,
	categoryID sql.NullInt64, name string, amount float64, paidDate, dueDate string, auto bool,
) (int64, bool, error) {
	tx, err := deps.DB.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var paymentID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO fin_biller_payments (user_id, biller_id, due_date, paid_date, amount, auto)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (biller_id, due_date) DO NOTHING RETURNING id`,
		uid, billerID, dueDate, paidDate, amount, auto).Scan(&paymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, true, tx.Commit() // cycle already recorded
		}
		return 0, false, err
	}

	desc := name
	if auto {
		desc = name + " (auto)"
	}
	var catArg any
	if categoryID.Valid {
		catArg = categoryID.Int64
	}
	// System-posted txn: no pocket (biller money is ambient — General).
	var txnID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at)
		 VALUES ($1,$2,$3,'expense',$4,$5,($6::timestamp AT TIME ZONE 'Asia/Kolkata')) RETURNING id`,
		uid, accountID, catArg, amount, desc, paidDate).Scan(&txnID); err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE fin_biller_payments SET txn_id = $1 WHERE id = $2`, txnID, paymentID); err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO fin_biller_payment_txns (payment_id, txn_id) VALUES ($1,$2)`,
		paymentID, txnID); err != nil {
		return 0, false, err
	}
	return txnID, false, tx.Commit()
}

// attachBillerTxns records a cycle as paid by linking pre-existing txns to
// it (no new txn). Payment amount = the explicit override or the sum of the
// attached txns. Returns alreadyPaid=true when the cycle was recorded before.
func attachBillerTxns(ctx context.Context, deps Deps, uid string, billerID int64,
	txnIDs []int64, amountOverride *float64, paidDate, dueDate string,
) (bool, error) {
	d := deps.DB
	// Verify ownership + sum in one shot; a count mismatch means a foreign
	// or missing txn id.
	var cnt int
	var sum float64
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(amount),0)
		FROM fin_transactions WHERE user_id = $1 AND id = ANY($2)`,
		uid, txnIDs).Scan(&cnt, &sum); err != nil {
		return false, err
	}
	if cnt != len(txnIDs) {
		return false, errors.New("one or more transactions not found")
	}
	amount := sum
	if amountOverride != nil {
		amount = *amountOverride
	}

	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var paymentID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO fin_biller_payments (user_id, biller_id, due_date, paid_date, amount, auto)
		 VALUES ($1,$2,$3,$4,$5,FALSE)
		 ON CONFLICT (biller_id, due_date) DO NOTHING RETURNING id`,
		uid, billerID, dueDate, paidDate, amount).Scan(&paymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, tx.Commit()
		}
		return false, err
	}
	for _, tid := range txnIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO fin_biller_payment_txns (payment_id, txn_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			paymentID, tid); err != nil {
			return false, err
		}
	}
	return false, tx.Commit()
}

// listBillerPayments returns the payment history for one biller with each
// cycle's linked txns (the link table plus the legacy single txn_id).
func listBillerPayments(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		type payTxn struct {
			ID          int64   `json:"id"`
			Amount      float64 `json:"amount"`
			Description string  `json:"description"`
			TxnAt       string  `json:"txn_at"`
			AccountName *string `json:"account_name"`
		}
		type payment struct {
			ID       int64    `json:"id"`
			DueDate  string   `json:"due_date"`
			PaidDate string   `json:"paid_date"`
			Amount   float64  `json:"amount"`
			Auto     bool     `json:"auto"`
			Txns     []payTxn `json:"txns"`
		}
		rows, err := d.Query(`SELECT id, due_date::text, paid_date::text, amount, auto
			FROM fin_biller_payments WHERE biller_id = $1 AND user_id = $2
			ORDER BY due_date DESC LIMIT 24`, id, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		out := []payment{}
		ids := []int64{}
		for rows.Next() {
			var p payment
			rows.Scan(&p.ID, &p.DueDate, &p.PaidDate, &p.Amount, &p.Auto)
			p.Txns = []payTxn{}
			out = append(out, p)
			ids = append(ids, p.ID)
		}
		rows.Close()
		if len(ids) > 0 {
			trows, terr := d.Query(`SELECT l.payment_id, t.id, t.amount, t.description, t.txn_at::text, a.name
				FROM (
					SELECT payment_id, txn_id FROM fin_biller_payment_txns WHERE payment_id = ANY($1)
					UNION
					SELECT p.id, p.txn_id FROM fin_biller_payments p
					WHERE p.id = ANY($1) AND p.txn_id IS NOT NULL
				) l
				JOIN fin_transactions t ON t.id = l.txn_id
				LEFT JOIN fin_accounts a ON a.id = t.account_id
				ORDER BY t.txn_at DESC`, ids)
			if terr == nil {
				byPayment := map[int64][]payTxn{}
				for trows.Next() {
					var pid int64
					var t payTxn
					trows.Scan(&pid, &t.ID, &t.Amount, &t.Description, &t.TxnAt, &t.AccountName)
					byPayment[pid] = append(byPayment[pid], t)
				}
				trows.Close()
				for i := range out {
					if txns, ok := byPayment[out[i].ID]; ok {
						out[i].Txns = txns
					}
				}
			}
		}
		writeJSON(w, 200, out)
	}
}

// --- alerts ---

func listBillerAlerts(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		onlyUnseen := queryParam(r, "unseen") == "true"
		q := `SELECT a.id, a.biller_id, b.name, a.kind, a.due_date::text, b.amount, a.seen, a.created_at::text
			FROM fin_biller_alerts a JOIN fin_billers b ON b.id = a.biller_id
			WHERE a.user_id = $1`
		if onlyUnseen {
			q += " AND a.seen = FALSE"
		}
		q += " ORDER BY a.created_at DESC LIMIT 50"
		rows, err := d.Query(q, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Alert struct {
			ID         int64   `json:"id"`
			BillerID   int64   `json:"biller_id"`
			BillerName string  `json:"biller_name"`
			Kind       string  `json:"kind"`
			DueDate    string  `json:"due_date"`
			Amount     float64 `json:"amount"`
			Seen       bool    `json:"seen"`
			CreatedAt  string  `json:"created_at"`
		}
		out := []Alert{}
		for rows.Next() {
			var a Alert
			rows.Scan(&a.ID, &a.BillerID, &a.BillerName, &a.Kind, &a.DueDate, &a.Amount, &a.Seen, &a.CreatedAt)
			out = append(out, a)
		}
		writeJSON(w, 200, out)
	}
}

func markBillerAlertSeen(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec(`UPDATE fin_biller_alerts SET seen = TRUE WHERE id = $1 AND user_id = $2`, id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// ProcessBillerCron walks every active biller for every user and:
//  1. for `auto_renew=true` rows whose due date has passed, posts the txn
//     and rolls next_due_date forward (catches up multiple missed cycles);
//  2. for rows within the alert_days window, inserts an "upcoming" alert.
//
// Idempotent: alerts are uniq on (biller_id, kind, due_date) and payments
// on (biller_id, due_date).
func ProcessBillerCron(ctx context.Context, deps Deps) (autoPosted int, upcomingNoticed int, err error) {
	d := deps.DB
	// Cron spans all users; every Sajni user is IST, so use the shared default
	// zone rather than the server's UTC clock for the due-date comparison.
	today := time.Now().In(defaultLoc).Format("2006-01-02")

	rows, err := d.QueryContext(ctx, `SELECT id, user_id, name, amount, frequency, next_due_date::text,
		COALESCE(anchor_day, EXTRACT(DAY FROM next_due_date)::INTEGER),
		account_id, category_id, auto_renew, remind_task, alert_days
		FROM fin_billers WHERE archived = FALSE`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	type bill struct {
		id                    int64
		userID                string
		name, freq, dueDate   string
		amount                float64
		accountID, categoryID sql.NullInt64
		autoRenew             bool
		remindTask            bool
		alertDays             int
		anchorDay             int
	}
	var bills []bill
	for rows.Next() {
		var b bill
		if err := rows.Scan(&b.id, &b.userID, &b.name, &b.amount, &b.freq, &b.dueDate, &b.anchorDay,
			&b.accountID, &b.categoryID, &b.autoRenew, &b.remindTask, &b.alertDays); err != nil {
			return 0, 0, fmt.Errorf("scan biller cron row: %w", err)
		}
		bills = append(bills, b)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate biller cron rows: %w", err)
	}

	todayT, _ := time.Parse("2006-01-02", today)
	var jobErrors []error
	for _, b := range bills {
		due, perr := time.Parse("2006-01-02", b.dueDate)
		if perr != nil {
			jobErrors = append(jobErrors, fmt.Errorf("biller %d has invalid next_due_date: %w", b.id, perr))
			continue
		}
		// 1) auto-renew (subscriptions only — bills never carry the flag):
		// catch up every cycle whose due date is <= today. A cycle the user
		// already paid manually just skips (alreadyPaid) and keeps rolling.
		if b.autoRenew && b.accountID.Valid {
			failed := false
			for !due.After(todayT) {
				_, alreadyPaid, terr := postBillerTxn(ctx, deps, b.userID, b.id, b.accountID.Int64, b.categoryID,
					b.name, b.amount, today, due.Format("2006-01-02"), true)
				if terr != nil {
					log.Warn().Err(terr).Int64("biller", b.id).Msg("biller auto-post failed")
					jobErrors = append(jobErrors, fmt.Errorf("post biller %d cycle: %w", b.id, terr))
					failed = true
					break
				}
				if !alreadyPaid {
					if _, err := d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
						VALUES ($1,$2,'auto_paid',$3) ON CONFLICT DO NOTHING`,
						b.userID, b.id, due.Format("2006-01-02")); err != nil {
						jobErrors = append(jobErrors, fmt.Errorf("record biller %d auto-paid alert: %w", b.id, err))
						failed = true
						break
					}
					autoPosted++
				}
				due = advanceDueDate(due, b.freq, b.anchorDay)
			}
			if failed {
				continue
			}
			if _, err := d.ExecContext(ctx, `UPDATE fin_billers SET next_due_date = $1, last_run_at = NOW() WHERE id = $2`,
				due.Format("2006-01-02"), b.id); err != nil {
				jobErrors = append(jobErrors, fmt.Errorf("advance biller %d due date: %w", b.id, err))
				continue
			}
			b.dueDate = due.Format("2006-01-02")
		}
		// 2) upcoming alert: due within alert_days but still in the future
		// (or today). Suppressed for auto-renew because they get auto_paid
		// alerts after the fact.
		if b.autoRenew {
			continue
		}
		gap := int(due.Sub(todayT).Hours() / 24)
		if gap >= 0 && gap <= b.alertDays {
			if b.remindTask {
				// Opt-in: spawn one bill-pay reminder task for this cycle.
				// The 'reminder_task' alert row (uniq on biller,kind,due) is
				// the idempotency sentinel — only create the task when the
				// insert actually took, so re-runs don't duplicate it. The
				// task itself carries the nudge, so we skip the 'upcoming'
				// alert for these billers.
				res, err := d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
					VALUES ($1,$2,'reminder_task',$3) ON CONFLICT DO NOTHING`,
					b.userID, b.id, due.Format("2006-01-02"))
				if err != nil {
					jobErrors = append(jobErrors, fmt.Errorf("record biller %d reminder alert: %w", b.id, err))
					continue
				}
				n, err := res.RowsAffected()
				if err != nil {
					jobErrors = append(jobErrors, fmt.Errorf("read biller %d reminder alert result: %w", b.id, err))
					continue
				}
				if n == 1 {
					spawnBillerTask(ctx, deps, b.userID, b.name, due)
					upcomingNoticed++
				}
			} else {
				res, err := d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
					VALUES ($1,$2,'upcoming',$3) ON CONFLICT DO NOTHING`,
					b.userID, b.id, due.Format("2006-01-02"))
				if err != nil {
					jobErrors = append(jobErrors, fmt.Errorf("record biller %d upcoming alert: %w", b.id, err))
					continue
				}
				n, err := res.RowsAffected()
				if err != nil {
					jobErrors = append(jobErrors, fmt.Errorf("read biller %d upcoming alert result: %w", b.id, err))
					continue
				}
				// First time this cycle's alert lands, nudge any registered
				// device. The in-app alert row stays the durable record; there
				// was never an email for this kind, so push is best-effort only.
				if n == 1 {
					when := "today"
					if gap == 1 {
						when = "tomorrow"
					} else if gap > 1 {
						when = fmt.Sprintf("in %d days", gap)
					}
					notifyPush(ctx, deps, b.userID, push.Notification{
						Title: "Bill due " + when,
						Body:  fmt.Sprintf("%s — ₹%.2f due %s", b.name, b.amount, due.Format("Jan 2")),
						Route: "/finance",
					})
				}
				upcomingNoticed++
			}
		}
	}
	return autoPosted, upcomingNoticed, errors.Join(jobErrors...)
}

// spawnBillerTask creates a "Pay {name}" reminder task for a biller's due
// cycle: scheduled at 09:00 on the due date in the user's local tz, with
// remind on so the reminder cron emails the morning-of nudge. Idempotency
// is the caller's responsibility (the reminder_task alert sentinel).
func spawnBillerTask(ctx context.Context, deps Deps, uid, name string, due time.Time) {
	d := deps.DB
	loc := userLocation(d, uid)
	sched := time.Date(due.Year(), due.Month(), due.Day(), 9, 0, 0, 0, loc)
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO tasks (user_id, title, priority, status, due_date, scheduled_at, remind)
		VALUES ($1, $2, 'high', 'todo', $3, $4, TRUE)
		RETURNING id`,
		uid, "Pay "+name, due.Format("2006-01-02"), sched.UTC()).Scan(&id)
	if err != nil {
		log.Warn().Err(err).Str("biller", name).Msg("biller reminder task insert failed")
		return
	}
	enqueueTaskReminderFromDB(ctx, d, uid, id)
}
