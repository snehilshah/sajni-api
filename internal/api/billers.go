package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
)

// Billers + subscriptions live alongside finance: every biller schedules a
// recurring expense against an account. The nightly tick (see
// ProcessBillerCron) emits "upcoming" alerts before the due date and, for
// auto-renew rows, posts the transaction once the date passes.

func registerBillerRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/finance/billers", listBillers(deps))
	mux.HandleFunc("POST /api/finance/billers", createBiller(deps))
	mux.HandleFunc("PUT /api/finance/billers/{id}", updateBiller(deps))
	mux.HandleFunc("DELETE /api/finance/billers/{id}", deleteBiller(deps))
	mux.HandleFunc("POST /api/finance/billers/{id}/pay", payBiller(deps))

	mux.HandleFunc("GET /api/finance/billers/alerts", listBillerAlerts(deps))
	mux.HandleFunc("POST /api/finance/billers/alerts/{id}/seen", markBillerAlertSeen(deps))
}

type billerResp struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Amount         float64 `json:"amount"`
	Frequency      string  `json:"frequency"`
	NextDueDate    string  `json:"next_due_date"`
	AccountID      *int64  `json:"account_id"`
	AccountName    *string `json:"account_name"`
	CategoryID     *int64  `json:"category_id"`
	CategoryName   *string `json:"category_name"`
	CategoryColor  *string `json:"category_color"`
	IsSubscription bool    `json:"is_subscription"`
	AutoRenew      bool    `json:"auto_renew"`
	RemindTask     bool    `json:"remind_task"`
	AlertDays      int     `json:"alert_days"`
	Color          string  `json:"color"`
	Notes          string  `json:"notes"`
	Archived       bool    `json:"archived"`
	LastPaidDate   *string `json:"last_paid_date"`
	CreatedAt      string  `json:"created_at"`
}

// validFrequency keeps the schema strict (we lean on this in advanceDueDate).
func validFrequency(f string) bool {
	switch f {
	case "weekly", "fortnightly", "monthly", "bimonthly":
		return true
	}
	return false
}

// advanceDueDate rolls a due date forward by one period.
func advanceDueDate(d time.Time, freq string) time.Time {
	switch freq {
	case "weekly":
		return d.AddDate(0, 0, 7)
	case "fortnightly":
		return d.AddDate(0, 0, 14)
	case "bimonthly":
		return d.AddDate(0, 2, 0)
	default: // monthly
		return d.AddDate(0, 1, 0)
	}
}

func listBillers(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		includeArchived := queryParam(r, "include_archived") == "true"

		q := `SELECT b.id, b.name, b.amount, b.frequency, b.next_due_date::text,
			b.account_id, a.name, b.category_id, c.name, c.color,
			b.is_subscription, b.auto_renew, b.remind_task, b.alert_days, b.color, b.notes, b.archived,
			(SELECT MAX(paid_date)::text FROM fin_biller_payments p WHERE p.biller_id = b.id),
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
			rows.Scan(&b.ID, &b.Name, &b.Amount, &b.Frequency, &b.NextDueDate,
				&b.AccountID, &b.AccountName, &b.CategoryID, &b.CategoryName, &b.CategoryColor,
				&b.IsSubscription, &b.AutoRenew, &b.RemindTask, &b.AlertDays, &b.Color, &b.Notes, &b.Archived,
				&b.LastPaidDate, &b.CreatedAt)
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
			Amount         float64 `json:"amount"`
			Frequency      string  `json:"frequency"`
			NextDueDate    string  `json:"next_due_date"`
			AccountID      *int64  `json:"account_id"`
			CategoryID     *int64  `json:"category_id"`
			IsSubscription bool    `json:"is_subscription"`
			AutoRenew      bool    `json:"auto_renew"`
			RemindTask     bool    `json:"remind_task"`
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
		if body.Frequency == "" {
			body.Frequency = "monthly"
		}
		if !validFrequency(body.Frequency) {
			errJSON(w, 400, "invalid frequency")
			return
		}
		if body.NextDueDate == "" {
			body.NextDueDate = time.Now().Format("2006-01-02")
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
		var id int64
		err := d.QueryRow(`INSERT INTO fin_billers
			(user_id, name, amount, frequency, next_due_date, account_id, category_id,
			 is_subscription, auto_renew, remind_task, alert_days, color, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
			uid, body.Name, body.Amount, body.Frequency, body.NextDueDate, body.AccountID, body.CategoryID,
			body.IsSubscription, body.AutoRenew, body.RemindTask, alertDays, body.Color, body.Notes).Scan(&id)
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
			Amount         *float64 `json:"amount"`
			Frequency      *string  `json:"frequency"`
			NextDueDate    *string  `json:"next_due_date"`
			AccountID      *int64   `json:"account_id"`
			CategoryID     *int64   `json:"category_id"`
			IsSubscription *bool    `json:"is_subscription"`
			AutoRenew      *bool    `json:"auto_renew"`
			RemindTask     *bool    `json:"remind_task"`
			AlertDays      *int     `json:"alert_days"`
			Color          *string  `json:"color"`
			Notes          *string  `json:"notes"`
			Archived       *bool    `json:"archived"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
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
			add("next_due_date", *body.NextDueDate)
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

// payBiller posts a transaction for the biller's current due cycle and
// rolls next_due_date forward. Idempotent on (biller_id, due_date).
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
			PaidDate string   `json:"paid_date"`
			Amount   *float64 `json:"amount"`
		}
		readJSON(r, &body)

		var name, freq string
		var amount float64
		var dueDate string
		var accountID, categoryID sql.NullInt64
		err = d.QueryRow(`SELECT name, amount, frequency, next_due_date::text, account_id, category_id
			FROM fin_billers WHERE id = $1 AND user_id = $2`,
			id, uid).Scan(&name, &amount, &freq, &dueDate, &accountID, &categoryID)
		if err != nil {
			errJSON(w, 404, "biller not found")
			return
		}
		if !accountID.Valid {
			errJSON(w, 400, "biller has no account; assign one before paying")
			return
		}
		if body.Amount != nil {
			amount = *body.Amount
		}
		paidDate := body.PaidDate
		if paidDate == "" {
			paidDate = time.Now().Format("2006-01-02")
		}

		txnID, perr := postBillerTxn(r.Context(), deps, uid, id, accountID.Int64, categoryID,
			name, amount, paidDate, dueDate, false)
		if perr != nil {
			errJSON(w, 500, perr.Error())
			return
		}

		due, _ := time.Parse("2006-01-02", dueDate)
		next := advanceDueDate(due, freq)
		d.Exec(`UPDATE fin_billers SET next_due_date = $1, updated_at = NOW(), last_run_at = NOW()
			WHERE id = $2 AND user_id = $3`, next.Format("2006-01-02"), id, uid)

		writeJSON(w, 200, map[string]any{
			"status":        "ok",
			"txn_id":        txnID,
			"next_due_date": next.Format("2006-01-02"),
		})
	}
}

// postBillerTxn inserts the transaction row, links it to the biller via
// fin_biller_payments, and is idempotent on (biller_id, due_date). Returns
// the txn id (0 if a duplicate payment row already existed).
func postBillerTxn(ctx context.Context, deps Deps, uid string, billerID, accountID int64,
	categoryID sql.NullInt64, name string, amount float64, paidDate, dueDate string, auto bool,
) (int64, error) {
	d := deps.DB
	// Skip if already posted for this cycle.
	var exists int
	d.QueryRowContext(ctx, `SELECT 1 FROM fin_biller_payments WHERE biller_id = $1 AND due_date = $2`,
		billerID, dueDate).Scan(&exists)
	if exists == 1 {
		return 0, nil
	}

	desc := name
	if auto {
		desc = name + " (auto)"
	}
	var txnID int64
	var catArg any
	if categoryID.Valid {
		catArg = categoryID.Int64
	}
	err := d.QueryRowContext(ctx,
		`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_date)
		 VALUES ($1,$2,$3,'expense',$4,$5,$6) RETURNING id`,
		uid, accountID, catArg, amount, desc, paidDate).Scan(&txnID)
	if err != nil {
		return 0, err
	}
	_, err = d.ExecContext(ctx,
		`INSERT INTO fin_biller_payments (user_id, biller_id, txn_id, due_date, paid_date, amount, auto)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (biller_id, due_date) DO NOTHING`,
		uid, billerID, txnID, dueDate, paidDate, amount, auto)
	return txnID, err
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
	today := time.Now().Format("2006-01-02")

	rows, err := d.QueryContext(ctx, `SELECT id, user_id, name, amount, frequency, next_due_date::text,
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
	}
	var bills []bill
	for rows.Next() {
		var b bill
		rows.Scan(&b.id, &b.userID, &b.name, &b.amount, &b.freq, &b.dueDate,
			&b.accountID, &b.categoryID, &b.autoRenew, &b.remindTask, &b.alertDays)
		bills = append(bills, b)
	}

	todayT, _ := time.Parse("2006-01-02", today)
	for _, b := range bills {
		due, perr := time.Parse("2006-01-02", b.dueDate)
		if perr != nil {
			continue
		}
		// 1) auto-renew: catch up every cycle whose due date is <= today.
		if b.autoRenew && b.accountID.Valid {
			for !due.After(todayT) {
				_, terr := postBillerTxn(ctx, deps, b.userID, b.id, b.accountID.Int64, b.categoryID,
					b.name, b.amount, today, due.Format("2006-01-02"), true)
				if terr != nil {
					log.Warn().Err(terr).Int64("biller", b.id).Msg("biller auto-post failed")
					break
				}
				// Notify the user the auto-charge happened.
				d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
					VALUES ($1,$2,'auto_paid',$3) ON CONFLICT DO NOTHING`,
					b.userID, b.id, due.Format("2006-01-02"))
				autoPosted++
				due = advanceDueDate(due, b.freq)
			}
			d.ExecContext(ctx, `UPDATE fin_billers SET next_due_date = $1, last_run_at = NOW() WHERE id = $2`,
				due.Format("2006-01-02"), b.id)
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
				res, _ := d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
					VALUES ($1,$2,'reminder_task',$3) ON CONFLICT DO NOTHING`,
					b.userID, b.id, due.Format("2006-01-02"))
				if n, _ := res.RowsAffected(); n == 1 {
					spawnBillerTask(ctx, d, b.userID, b.name, due)
					upcomingNoticed++
				}
			} else {
				d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
					VALUES ($1,$2,'upcoming',$3) ON CONFLICT DO NOTHING`,
					b.userID, b.id, due.Format("2006-01-02"))
				upcomingNoticed++
			}
		}
	}
	return autoPosted, upcomingNoticed, nil
}

// spawnBillerTask creates a "Pay {name}" reminder task for a biller's due
// cycle: scheduled at 09:00 on the due date in the user's local tz, with
// remind on so the reminder cron emails the morning-of nudge. Idempotency
// is the caller's responsibility (the reminder_task alert sentinel).
func spawnBillerTask(ctx context.Context, d *db.DB, uid, name string, due time.Time) {
	loc := userLocation(d, uid)
	sched := time.Date(due.Year(), due.Month(), due.Day(), 9, 0, 0, 0, loc)
	d.ExecContext(ctx, `
		INSERT INTO tasks (user_id, title, priority, status, due_date, scheduled_at, remind)
		VALUES ($1, $2, 'high', 'todo', $3, $4, TRUE)`,
		uid, "Pay "+name, due.Format("2006-01-02"), sched.UTC())
}
