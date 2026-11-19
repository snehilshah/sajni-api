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

	"sajni/internal/db"
	"sajni/internal/push"
)

// Billers are recurring items (db/recurring.go has the shared logic): kind
// 'subscription' = a fixed amount, 'bill' = it varies (the amount is an
// estimate); income = money expected in (Salary). An item waits for its
// payment, which attaches itself when it fits (db.MatchRecurring); auto_renew
// posts it instead. The sweep (ProcessBillerCron) raises "upcoming" alerts,
// posts auto-renew cycles and nudges at 8 pm on a due day with nothing paid.

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
	AutoRenew      bool     `json:"auto_renew"`
	RemindTask     bool     `json:"remind_task"`
	AlertDays      int      `json:"alert_days"`
	Color          string   `json:"color"`
	Notes          string   `json:"notes"`
	Archived       bool     `json:"archived"`
	Income         bool     `json:"income"`
	LastPaidDate   *string  `json:"last_paid_date"`
	LastPaidAmount *float64 `json:"last_paid_amount"`
	CreatedAt      string   `json:"created_at"`
}

func validBillerKind(k string) bool {
	return k == "subscription" || k == "bill"
}

func validFrequency(f string) bool { return db.ValidRecurringFrequency(f) }

func listBillers(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		includeArchived := queryParam(r, "include_archived") == "true"

		q := `SELECT b.id, b.name, b.kind, b.amount, b.frequency, b.next_due_date::text,
			b.account_id, a.name, b.category_id, c.name, c.color,
			b.auto_renew, b.remind_task, b.alert_days, b.color, b.notes, b.archived, b.income,
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
				&b.AutoRenew, &b.RemindTask, &b.AlertDays, &b.Color, &b.Notes, &b.Archived, &b.Income,
				&b.LastPaidDate, &b.LastPaidAmount, &b.CreatedAt)
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
			Name        string  `json:"name"`
			Kind        string  `json:"kind"`
			Amount      float64 `json:"amount"`
			Frequency   string  `json:"frequency"`
			NextDueDate string  `json:"next_due_date"`
			AccountID   *int64  `json:"account_id"`
			CategoryID  *int64  `json:"category_id"`
			AutoRenew   bool    `json:"auto_renew"`
			RemindTask  bool    `json:"remind_task"`
			AlertDays   *int    `json:"alert_days"`
			Color       string  `json:"color"`
			Notes       string  `json:"notes"`
			Income      bool    `json:"income"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			errJSON(w, 400, "Give the biller a name.")
			return
		}
		if body.Kind == "" {
			body.Kind = "subscription"
		}
		if !validBillerKind(body.Kind) {
			errJSON(w, 400, "Pick subscription or bill.")
			return
		}
		// Subscriptions have a fixed price; bills carry an optional estimate.
		if body.Kind == "subscription" && body.Amount <= 0 {
			errJSON(w, 400, "A subscription needs a fixed amount.")
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
			errJSON(w, 400, "Pick how often it repeats.")
			return
		}
		if body.NextDueDate == "" {
			body.NextDueDate = userNow(d, uid).Format("2006-01-02")
		}
		due, err := time.Parse("2006-01-02", body.NextDueDate)
		if err != nil {
			errJSON(w, 400, "Pick a valid next due date.")
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
			errJSON(w, 400, "Auto-renew needs an account to charge.")
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
			 auto_renew, remind_task, alert_days, color, notes, income)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
			uid, body.Name, body.Kind, body.Amount, body.Frequency, body.NextDueDate, due.Day(), body.AccountID, body.CategoryID,
			body.AutoRenew, body.RemindTask, alertDays, body.Color, body.Notes, body.Income).Scan(&id)
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
			Name        *string  `json:"name"`
			Kind        *string  `json:"kind"`
			Amount      *float64 `json:"amount"`
			Frequency   *string  `json:"frequency"`
			NextDueDate *string  `json:"next_due_date"`
			AccountID   *int64   `json:"account_id"`
			CategoryID  *int64   `json:"category_id"`
			AutoRenew   *bool    `json:"auto_renew"`
			RemindTask  *bool    `json:"remind_task"`
			AlertDays   *int     `json:"alert_days"`
			Color       *string  `json:"color"`
			Notes       *string  `json:"notes"`
			Archived    *bool    `json:"archived"`
			Income      *bool    `json:"income"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Kind != nil && !validBillerKind(*body.Kind) {
			errJSON(w, 400, "Pick subscription or bill.")
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
			errJSON(w, 400, "A subscription needs a fixed amount.")
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
		}
		if body.Amount != nil {
			add("amount", *body.Amount)
		}
		if body.Frequency != nil {
			if !validFrequency(*body.Frequency) {
				errJSON(w, 400, "Pick how often it repeats.")
				return
			}
			add("frequency", *body.Frequency)
		}
		if body.NextDueDate != nil {
			due, err := time.Parse("2006-01-02", *body.NextDueDate)
			if err != nil {
				errJSON(w, 400, "Pick a valid next due date.")
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
		if body.Income != nil {
			add("income", *body.Income)
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

// payBiller records one cycle of an item (db.PayRecurring): it posts a row
// (`amount` overrides the stored one, a bill's actual) or, with
// attach_txn_ids, links rows that already exist and posts nothing. due_date
// is the cycle the client showed; a second tap on it reads already_paid
// instead of paying next month too.
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
			DueDate      string   `json:"due_date"`
			Amount       *float64 `json:"amount"`
			AttachTxnIDs []int64  `json:"attach_txn_ids"`
		}
		readJSON(r, &body)
		tx, err := d.BeginTx(r.Context(), nil)
		if err != nil {
			internalError(w, r, "begin pay", err)
			return
		}
		defer tx.Rollback()
		paid, err := db.PayRecurring(r.Context(), tx, uid, id, db.RecurringPay{
			DueDate: body.DueDate, PaidDate: body.PaidDate, Amount: body.Amount, AttachTxnIDs: body.AttachTxnIDs,
		})
		if errors.Is(err, db.ErrRecurringNotFound) {
			errJSON(w, 404, "biller not found")
			return
		}
		if err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit pay", err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"status":        "ok",
			"txn_id":        paid.TxnID,
			"already_paid":  paid.AlreadyPaid,
			"next_due_date": paid.NextDue,
		})
	}
}

// listBillerPayments returns the payment history for one biller with each
// cycle's linked txns.
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
				FROM fin_biller_payment_txns l
				JOIN fin_transactions t ON t.id = l.txn_id
				LEFT JOIN fin_accounts a ON a.id = t.account_id
				WHERE l.payment_id = ANY($1)
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

// ProcessBillerCron walks every active recurring item for every user and:
//  1. auto_renew items: posts each cycle due by today (dated its own due
//     day), never one due before the item existed;
//  2. from 9 am, raises an "upcoming" alert alert_days before a payment's
//     due date (0 = the morning of the due day);
//  3. at 8 pm on a due day (or later, inside the cycle's window) with
//     nothing paid for it, nudges once: Home Contrib isn't recorded yet.
//
// Idempotent: alerts are unique on (biller_id, kind, due_date) and payments
// on (biller_id, due_date).
func ProcessBillerCron(ctx context.Context, deps Deps) (autoPosted int, upcomingNoticed int, err error) {
	d := deps.DB
	runAt := time.Now()

	rows, err := d.QueryContext(ctx, `SELECT b.id, b.user_id, b.name, b.amount, b.frequency, b.next_due_date::text,
		COALESCE(b.anchor_day, EXTRACT(DAY FROM b.next_due_date)::INTEGER),
		b.account_id, b.auto_renew, b.remind_task, b.alert_days, b.income,
		COALESCE(u.timezone,''), b.created_at
		FROM fin_billers b
		JOIN users u ON u.id = b.user_id
		WHERE b.archived = FALSE AND u.deleted_at IS NULL`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	type bill struct {
		id                  int64
		userID              string
		name, freq, dueDate string
		amount              float64
		accountID           sql.NullInt64
		autoRenew           bool
		remindTask          bool
		income              bool
		alertDays           int
		anchorDay           int
		timezone            string
		created             time.Time
	}
	var bills []bill
	for rows.Next() {
		var b bill
		if err := rows.Scan(&b.id, &b.userID, &b.name, &b.amount, &b.freq, &b.dueDate, &b.anchorDay,
			&b.accountID, &b.autoRenew, &b.remindTask, &b.alertDays, &b.income, &b.timezone, &b.created); err != nil {
			return 0, 0, fmt.Errorf("scan biller cron row: %w", err)
		}
		bills = append(bills, b)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate biller cron rows: %w", err)
	}

	var jobErrors []error
	for _, b := range bills {
		loc := timezoneLocation(b.timezone)
		local := runAt.In(loc)
		todayT, _ := time.Parse("2006-01-02", local.Format("2006-01-02"))
		createdT, _ := time.Parse("2006-01-02", b.created.In(loc).Format("2006-01-02"))
		due, perr := time.Parse("2006-01-02", b.dueDate)
		if perr != nil {
			jobErrors = append(jobErrors, fmt.Errorf("biller %d has invalid next_due_date: %w", b.id, perr))
			continue
		}
		// 1) Posts by itself. A cycle due before the item was made is skipped,
		// not back-posted; a cycle already paid (by hand, or by a payment that
		// attached itself) just moves on.
		if b.autoRenew && b.accountID.Valid {
			failed := false
			for !due.After(todayT) {
				if due.Before(createdT) {
					due = db.AdvanceDue(due, b.freq, b.anchorDay)
					continue
				}
				day := due.Format("2006-01-02")
				tx, terr := d.BeginTx(ctx, nil)
				var paid db.RecurringPaid
				if terr == nil {
					paid, terr = db.PayRecurring(ctx, tx, b.userID, b.id, db.RecurringPay{DueDate: day, PaidDate: day, Auto: true})
					if terr == nil {
						terr = tx.Commit()
					} else {
						tx.Rollback()
					}
				}
				if terr != nil {
					log.Warn().Err(terr).Int64("biller", b.id).Msg("biller auto-post failed")
					jobErrors = append(jobErrors, fmt.Errorf("post biller %d cycle: %w", b.id, terr))
					failed = true
					break
				}
				if !paid.AlreadyPaid {
					if _, err := d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
						VALUES ($1,$2,'auto_paid',$3) ON CONFLICT DO NOTHING`, b.userID, b.id, day); err != nil {
						jobErrors = append(jobErrors, fmt.Errorf("record biller %d auto-paid alert: %w", b.id, err))
					}
					autoPosted++
				}
				due = db.AdvanceDue(due, b.freq, b.anchorDay)
			}
			if failed {
				continue
			}
			if _, err := d.ExecContext(ctx, `UPDATE fin_billers SET next_due_date = $1, last_run_at = NOW() WHERE id = $2 AND next_due_date < $1`,
				due.Format("2006-01-02"), b.id); err != nil {
				jobErrors = append(jobErrors, fmt.Errorf("advance biller %d due date: %w", b.id, err))
			}
			continue // auto_paid alerts say it after the fact
		}

		// 3) Nothing recorded by 8 pm on the due day: one nudge for the cycle.
		cycle := db.RecurringCycleOn(due, b.freq, b.anchorDay, todayT)
		late := todayT.After(cycle) && !todayT.After(cycle.AddDate(0, 0, db.RecurringAfter))
		if (todayT.Equal(cycle) && local.Hour() >= 20) || late {
			var paid bool
			d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_biller_payments WHERE biller_id = $1 AND due_date = $2)`,
				b.id, cycle.Format("2006-01-02")).Scan(&paid)
			if !paid {
				res, err := d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
					VALUES ($1,$2,'missed',$3) ON CONFLICT DO NOTHING`, b.userID, b.id, cycle.Format("2006-01-02"))
				if err != nil {
					jobErrors = append(jobErrors, fmt.Errorf("record biller %d missed alert: %w", b.id, err))
					continue
				}
				if n, _ := res.RowsAffected(); n == 1 {
					notifyPush(ctx, deps, b.userID, recurringNudge(b.name, b.amount, b.income, cycle, todayT))
					upcomingNoticed++
				}
			}
		}

		// 2) Upcoming: due within alert_days, from 9 am (the first sweep after
		// midnight would wake the owner). Money expected in needs no warning.
		if b.income || local.Hour() < 9 {
			continue
		}
		gap := int(due.Sub(todayT).Hours() / 24)
		if gap >= 0 && gap <= b.alertDays {
			if b.remindTask {
				// Opt-in: one bill-pay reminder task per cycle; the
				// 'reminder_task' alert row is the idempotency sentinel.
				res, err := d.ExecContext(ctx, `INSERT INTO fin_biller_alerts (user_id, biller_id, kind, due_date)
					VALUES ($1,$2,'reminder_task',$3) ON CONFLICT DO NOTHING`,
					b.userID, b.id, due.Format("2006-01-02"))
				if err != nil {
					jobErrors = append(jobErrors, fmt.Errorf("record biller %d reminder alert: %w", b.id, err))
					continue
				}
				if n, _ := res.RowsAffected(); n == 1 {
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
				// First time this cycle's alert lands, nudge any registered
				// device; the alert row is the durable record.
				if n, _ := res.RowsAffected(); n == 1 {
					when := "today"
					if gap == 1 {
						when = "tomorrow"
					} else if gap > 1 {
						when = fmt.Sprintf("in %d days", gap)
					}
					notifyPush(ctx, deps, b.userID, push.Notification{
						Type:  push.TypeBillDue,
						Title: "Bill due " + when,
						Body:  fmt.Sprintf("%s · ₹%s due %s", b.name, formatINR(b.amount), due.Format("Jan 2")),
						Route: "/finance/billers",
					})
					upcomingNoticed++
				}
			}
		}
	}
	return autoPosted, upcomingNoticed, errors.Join(jobErrors...)
}

// recurringNudge words the 8 pm nudge for a cycle with nothing recorded.
func recurringNudge(name string, amount float64, income bool, cycle, today time.Time) push.Notification {
	when := "was due today"
	if today.After(cycle) {
		when = "was due " + cycle.Format("Jan 2")
	}
	if income {
		when = strings.Replace(when, "was due", "was expected", 1)
	}
	body := fmt.Sprintf("₹%s %s. Add it once it's done.", formatINR(amount), when)
	if amount <= 0 {
		body = strings.ToUpper(when[:1]) + when[1:] + ". Add it once it's done."
	}
	return push.Notification{
		Type:  push.TypeBillDue,
		Title: name + " isn't recorded yet",
		Body:  body,
		Route: "/finance/billers",
	}
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
	enqueueTaskReminderFromDB(ctx, d, deps.ReminderQueue, uid, id)
}
