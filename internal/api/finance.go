package api

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sajni/internal/db"
)

var errFinanceReferenceNotFound = errors.New("finance reference not found")

// dbtx is deliberately the small SQL surface used by finance mutations. Both
// *db.DB and *sql.Tx satisfy it.
type dbtx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func requireOwnedFinanceRef(ctx context.Context, q dbtx, table, userID string, id int64) error {
	if id == 0 {
		return errFinanceReferenceNotFound
	}
	allowed := map[string]bool{
		"fin_accounts": true, "fin_categories": true, "fin_pockets": true,
		"fin_budgets": true, "fin_transactions": true,
	}
	if !allowed[table] {
		return fmt.Errorf("unsupported finance reference %q", table)
	}
	var owned bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE id=$1 AND user_id=$2)", id, userID).Scan(&owned); err != nil {
		return fmt.Errorf("check %s ownership: %w", table, err)
	}
	if !owned {
		return errFinanceReferenceNotFound
	}
	return nil
}

// requirePersonalPocket is the pocket variant of requireOwnedFinanceRef:
// personal-ledger machinery (txn pocket refs, budget filters, active pocket)
// must never point at a shared pocket.
func requirePersonalPocket(ctx context.Context, q dbtx, userID string, id int64) error {
	if id == 0 {
		return errFinanceReferenceNotFound
	}
	var ok bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_pockets WHERE id=$1 AND user_id=$2 AND kind='personal')`, id, userID).Scan(&ok); err != nil {
		return fmt.Errorf("check personal pocket: %w", err)
	}
	if !ok {
		return errFinanceReferenceNotFound
	}
	return nil
}

func registerFinanceRoutes(mux *http.ServeMux, deps Deps) {
	// Accounts
	mux.HandleFunc("GET /api/finance/accounts", listAccounts(deps))
	mux.HandleFunc("POST /api/finance/accounts", createAccount(deps))
	mux.HandleFunc("PUT /api/finance/accounts/{id}", updateAccount(deps))
	mux.HandleFunc("DELETE /api/finance/accounts/{id}", deleteAccount(deps))

	// Categories
	mux.HandleFunc("GET /api/finance/categories", listCategories(deps))
	mux.HandleFunc("POST /api/finance/categories", createCategory(deps))
	mux.HandleFunc("PUT /api/finance/categories/{id}", updateCategory(deps))
	mux.HandleFunc("DELETE /api/finance/categories/{id}", deleteCategory(deps))

	// Transactions
	mux.HandleFunc("GET /api/finance/transactions", listTransactions(deps))
	mux.HandleFunc("POST /api/finance/transactions", createTransaction(deps))
	mux.HandleFunc("PUT /api/finance/transactions/{id}", updateTransaction(deps))
	mux.HandleFunc("DELETE /api/finance/transactions/{id}", deleteTransaction(deps))

	// Pockets (spend contexts)
	registerPocketRoutes(mux, deps)

	// Budgets
	mux.HandleFunc("GET /api/finance/budgets", listBudgets(deps))
	mux.HandleFunc("POST /api/finance/budgets", createBudget(deps))
	mux.HandleFunc("PUT /api/finance/budgets/{id}", updateBudget(deps))
	mux.HandleFunc("DELETE /api/finance/budgets/{id}", deleteBudget(deps))

	// Investments
	mux.HandleFunc("GET /api/finance/investments", listInvestments(deps))
	mux.HandleFunc("POST /api/finance/investments", createInvestment(deps))
	mux.HandleFunc("PUT /api/finance/investments/{id}", updateInvestment(deps))
	mux.HandleFunc("DELETE /api/finance/investments/{id}", deleteInvestment(deps))

	// Virtual savings
	mux.HandleFunc("GET /api/finance/savings", listSavings(deps))
	mux.HandleFunc("POST /api/finance/savings", createSaving(deps))
	mux.HandleFunc("PUT /api/finance/savings/{id}", updateSaving(deps))
	mux.HandleFunc("DELETE /api/finance/savings/{id}", deleteSaving(deps))

	// Credit card statements
	mux.HandleFunc("GET /api/finance/cards/statements", listStatements(deps))
	mux.HandleFunc("POST /api/finance/cards/{id}/statement-preview", previewStatement(deps))
	mux.HandleFunc("POST /api/finance/cards/{id}/statements", createStatement(deps))
	mux.HandleFunc("PUT /api/finance/cards/statements/{id}", updateStatement(deps))
	mux.HandleFunc("DELETE /api/finance/cards/statements/{id}", deleteStatement(deps))

	// Overview / analytics
	mux.HandleFunc("GET /api/finance/overview", financeOverview(deps))
	mux.HandleFunc("GET /api/finance/networth/history", networthHistory(deps))
	mux.HandleFunc("POST /api/finance/networth/snapshot", networthSnapshot(deps))

	// Export
	mux.HandleFunc("GET /api/finance/export/transactions.csv", exportTransactionsCSV(deps))
	mux.HandleFunc("GET /api/finance/export/budgets.csv", exportBudgetsCSV(deps))
	mux.HandleFunc("GET /api/finance/export/networth.csv", exportNetworthCSV(deps))

	// AI-assisted category inference for transaction titles.
	mux.HandleFunc("POST /api/finance/categorize", categorizeTransaction(deps))
	// AI parse of a shared bank/UPI message into transaction fields (PWA share target).
	mux.HandleFunc("POST /api/finance/parse-message", parseTransactionMessage(deps))
}

// --- helpers ---------------------------------------------------------------

var defaultExpenseCategories = []struct {
	Name  string
	Color string
	Icon  string
}{
	{"Food & Dining", "#F97316", "utensils"},
	{"Transport", "#3B82F6", "car"},
	{"Bills & Utilities", "#06B6D4", "plug"},
	{"Shopping", "#EC4899", "shopping-bag"},
	{"Others", "#6B7280", "circle"},
}

var defaultIncomeCategories = []struct {
	Name  string
	Color string
	Icon  string
}{
	{"Salary", "#10B981", "wallet"},
	{"Interest", "#22C55E", "trending-up"},
	{"Others", "#6B7280", "circle"},
}

func ensureDefaultCategories(deps Deps, uid string) {
	d := deps.DB
	for _, c := range defaultExpenseCategories {
		d.Exec("INSERT INTO fin_categories (user_id, name, kind, color, icon) VALUES ($1, $2, 'expense', $3, $4) ON CONFLICT DO NOTHING", uid, c.Name, c.Color, c.Icon)
	}
	for _, c := range defaultIncomeCategories {
		d.Exec("INSERT INTO fin_categories (user_id, name, kind, color, icon) VALUES ($1, $2, 'income', $3, $4) ON CONFLICT DO NOTHING", uid, c.Name, c.Color, c.Icon)
	}
}

func categoryNameKey(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "other" || key == "others" {
		return "others"
	}
	return key
}

func isDefaultCategoryName(kind, name string) bool {
	key := categoryNameKey(name)
	var defaults []struct {
		Name  string
		Color string
		Icon  string
	}
	if kind == "income" {
		defaults = defaultIncomeCategories
	} else if kind == "expense" {
		defaults = defaultExpenseCategories
	}
	for _, category := range defaults {
		if categoryNameKey(category.Name) == key {
			return true
		}
	}
	return false
}

func categoryNameExists(d *db.DB, uid, kind, name string, excludeID int64) bool {
	var exists bool
	d.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM fin_categories
			 WHERE user_id=$1 AND kind=$2 AND id<>$3
			   AND CASE WHEN LOWER(BTRIM(name)) IN ('other','others') THEN 'others' ELSE LOWER(BTRIM(name)) END = $4
		)`, uid, kind, excludeID, categoryNameKey(name)).Scan(&exists)
	return exists
}

// computeBalance returns the current signed balance of an account based on
// opening_balance + income - expense + transfer_in - transfer_out.
// For credit cards this comes out negative when money is owed.
func computeBalance(deps Deps, uid string, accountID int64) float64 {
	d := deps.DB
	var opening float64
	d.QueryRow("SELECT opening_balance FROM fin_accounts WHERE id = $1 AND user_id = $2", accountID, uid).Scan(&opening)

	var income, expense, transferIn, transferOut float64
	d.QueryRow("SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND account_id = $2 AND type = 'income'", uid, accountID).Scan(&income)
	d.QueryRow("SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND account_id = $2 AND type = 'expense'", uid, accountID).Scan(&expense)
	d.QueryRow("SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND account_id = $2 AND type = 'transfer_in'", uid, accountID).Scan(&transferIn)
	d.QueryRow("SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND account_id = $2 AND type = 'transfer_out'", uid, accountID).Scan(&transferOut)

	return opening + income - expense + transferIn - transferOut
}

// --- accounts --------------------------------------------------------------

type accountResp struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Institution    string   `json:"institution"`
	Currency       string   `json:"currency"`
	OpeningBalance float64  `json:"opening_balance"`
	Balance        float64  `json:"balance"`
	CreditLimit    *float64 `json:"credit_limit"`
	StatementDay   *int     `json:"statement_day"`
	DueDay         *int     `json:"due_day"`
	CashbackType   string   `json:"cashback_type"`
	CashbackValue  float64  `json:"cashback_value"`
	SalaryAmount   float64  `json:"salary_amount"`
	SalaryDay      *int     `json:"salary_day"`
	MatchHints     string   `json:"match_hints"`
	Color          string   `json:"color"`
	Archived       bool     `json:"archived"`
	CreatedAt      string   `json:"created_at"`
}

func listAccounts(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`SELECT id, name, type, institution, currency, opening_balance,
			credit_limit, statement_day, due_day, cashback_type, cashback_value, salary_amount, salary_day, match_hints, color, archived, created_at
			FROM fin_accounts WHERE user_id = $1 ORDER BY archived ASC, created_at ASC`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		var out []accountResp
		for rows.Next() {
			var a accountResp
			rows.Scan(&a.ID, &a.Name, &a.Type, &a.Institution, &a.Currency, &a.OpeningBalance,
				&a.CreditLimit, &a.StatementDay, &a.DueDay, &a.CashbackType, &a.CashbackValue, &a.SalaryAmount, &a.SalaryDay, &a.MatchHints, &a.Color, &a.Archived, &a.CreatedAt)
			a.Balance = computeBalance(deps, uid, a.ID)
			out = append(out, a)
		}
		if out == nil {
			out = []accountResp{}
		}
		writeJSON(w, 200, out)
	}
}

func createAccount(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			Name           string   `json:"name"`
			Type           string   `json:"type"`
			Institution    string   `json:"institution"`
			Currency       string   `json:"currency"`
			OpeningBalance float64  `json:"opening_balance"`
			CreditLimit    *float64 `json:"credit_limit"`
			StatementDay   *int     `json:"statement_day"`
			DueDay         *int     `json:"due_day"`
			CashbackType   string   `json:"cashback_type"`
			CashbackValue  float64  `json:"cashback_value"`
			SalaryAmount   float64  `json:"salary_amount"`
			SalaryDay      *int     `json:"salary_day"`
			MatchHints     string   `json:"match_hints"`
			Color          string   `json:"color"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Type == "" {
			b.Type = "savings"
		}
		if b.Currency == "" {
			b.Currency = "INR"
		}
		if b.CashbackType == "" {
			b.CashbackType = "none"
		}
		if b.Color == "" {
			b.Color = "#2D5A4F"
		}
		var id int64
		err := d.QueryRow(`INSERT INTO fin_accounts
			(user_id, name, type, institution, currency, opening_balance, credit_limit, statement_day, due_day, cashback_type, cashback_value, salary_amount, salary_day, match_hints, color)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
			uid, b.Name, b.Type, b.Institution, b.Currency, b.OpeningBalance,
			b.CreditLimit, b.StatementDay, b.DueDay, b.CashbackType, b.CashbackValue, b.SalaryAmount, b.SalaryDay, b.MatchHints, b.Color,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateAccount(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			Name           *string  `json:"name"`
			Type           *string  `json:"type"`
			Institution    *string  `json:"institution"`
			Currency       *string  `json:"currency"`
			OpeningBalance *float64 `json:"opening_balance"`
			CreditLimit    *float64 `json:"credit_limit"`
			StatementDay   *int     `json:"statement_day"`
			DueDay         *int     `json:"due_day"`
			CashbackType   *string  `json:"cashback_type"`
			CashbackValue  *float64 `json:"cashback_value"`
			SalaryAmount   *float64 `json:"salary_amount"`
			SalaryDay      *int     `json:"salary_day"`
			MatchHints     *string  `json:"match_hints"`
			Color          *string  `json:"color"`
			Archived       *bool    `json:"archived"`
		}
		if err := readJSON(r, &b); err != nil {
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
		if b.Name != nil {
			add("name", *b.Name)
		}
		if b.Type != nil {
			add("type", *b.Type)
		}
		if b.Institution != nil {
			add("institution", *b.Institution)
		}
		if b.Currency != nil {
			add("currency", *b.Currency)
		}
		if b.OpeningBalance != nil {
			add("opening_balance", *b.OpeningBalance)
		}
		if b.CreditLimit != nil {
			add("credit_limit", *b.CreditLimit)
		}
		if b.StatementDay != nil {
			add("statement_day", *b.StatementDay)
		}
		if b.DueDay != nil {
			add("due_day", *b.DueDay)
		}
		if b.CashbackType != nil {
			add("cashback_type", *b.CashbackType)
		}
		if b.CashbackValue != nil {
			add("cashback_value", *b.CashbackValue)
		}
		if b.SalaryAmount != nil {
			add("salary_amount", *b.SalaryAmount)
		}
		if b.SalaryDay != nil {
			add("salary_day", *b.SalaryDay)
		}
		if b.MatchHints != nil {
			add("match_hints", *b.MatchHints)
		}
		if b.Color != nil {
			add("color", *b.Color)
		}
		if b.Archived != nil {
			add("archived", *b.Archived)
		}
		args = append(args, id, uid)
		q := "UPDATE fin_accounts SET " + strings.Join(set, ", ") + " WHERE id = $" + itoa(ph) + " AND user_id = $" + itoa(ph+1)
		if _, err := d.Exec(q, args...); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteAccount(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM fin_accounts WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- categories ------------------------------------------------------------

func listCategories(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		ensureDefaultCategories(deps, uid)
		kind := queryParam(r, "kind")
		args := []any{uid}
		q := "SELECT id, name, kind, color, icon FROM fin_categories WHERE user_id = $1"
		if kind != "" {
			q += " AND kind = $2"
			args = append(args, kind)
		}
		q += " ORDER BY kind, name"
		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Cat struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Kind  string `json:"kind"`
			Color string `json:"color"`
			Icon  string `json:"icon"`
		}
		var out []Cat
		for rows.Next() {
			var c Cat
			rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Color, &c.Icon)
			out = append(out, c)
		}
		if out == nil {
			out = []Cat{}
		}
		writeJSON(w, 200, out)
	}
}

func createCategory(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			Name  string `json:"name"`
			Kind  string `json:"kind"`
			Color string `json:"color"`
			Icon  string `json:"icon"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Kind == "" {
			b.Kind = "expense"
		}
		b.Name = strings.TrimSpace(b.Name)
		if b.Name == "" {
			errJSON(w, 400, "category name is required")
			return
		}
		if b.Kind != "expense" && b.Kind != "income" {
			errJSON(w, 400, "category kind must be expense or income")
			return
		}
		if isDefaultCategoryName(b.Kind, b.Name) || categoryNameExists(d, uid, b.Kind, b.Name, 0) {
			errJSON(w, 409, "category already exists or is predefined for this type")
			return
		}
		if b.Color == "" {
			b.Color = "#6B7280"
		}
		var id int64
		err := d.QueryRow(
			"INSERT INTO fin_categories (user_id, name, kind, color, icon) VALUES ($1,$2,$3,$4,$5) RETURNING id",
			uid, b.Name, b.Kind, b.Color, b.Icon,
		).Scan(&id)
		if err != nil {
			if categoryNameExists(d, uid, b.Kind, b.Name, 0) {
				errJSON(w, 409, "category already exists for this type")
			} else {
				errJSON(w, 500, err.Error())
			}
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateCategory(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			Name  *string `json:"name"`
			Color *string `json:"color"`
			Icon  *string `json:"icon"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Name != nil {
			var currentName, kind string
			if err := d.QueryRow("SELECT name, kind FROM fin_categories WHERE id=$1 AND user_id=$2", id, uid).Scan(&currentName, &kind); err != nil {
				errJSON(w, 404, "category not found")
				return
			}
			name := strings.TrimSpace(*b.Name)
			if name == "" {
				errJSON(w, 400, "category name is required")
				return
			}
			if categoryNameExists(d, uid, kind, name, id) || (categoryNameKey(currentName) != categoryNameKey(name) && isDefaultCategoryName(kind, name)) {
				errJSON(w, 409, "category already exists or is predefined for this type")
				return
			}
			if _, err := d.Exec("UPDATE fin_categories SET name = $1 WHERE id = $2 AND user_id = $3", name, id, uid); err != nil {
				if categoryNameExists(d, uid, kind, name, id) {
					errJSON(w, 409, "category already exists for this type")
				} else {
					errJSON(w, 500, err.Error())
				}
				return
			}
		}
		if b.Color != nil {
			d.Exec("UPDATE fin_categories SET color = $1 WHERE id = $2 AND user_id = $3", *b.Color, id, uid)
		}
		if b.Icon != nil {
			d.Exec("UPDATE fin_categories SET icon = $1 WHERE id = $2 AND user_id = $3", *b.Icon, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteCategory(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM fin_categories WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- transactions ----------------------------------------------------------

type txnResp struct {
	ID            int64   `json:"id"`
	AccountID     int64   `json:"account_id"`
	AccountName   string  `json:"account_name"`
	CategoryID    *int64  `json:"category_id"`
	CategoryName  *string `json:"category_name"`
	CategoryColor *string `json:"category_color"`
	Type          string  `json:"type"`
	Amount        float64 `json:"amount"`
	Description   string  `json:"description"`
	Note          string  `json:"note"`
	TxnAt         string  `json:"txn_at"` // RFC3339 in IST (carries +05:30)
	TransferPair  *int64  `json:"transfer_pair"`
	LinkedAccount *int64  `json:"linked_account"`
	PocketID      *int64  `json:"pocket_id"`         // NULL = General
	PocketName    *string `json:"pocket_name"`       // joined for display
	SharedExpense *int64  `json:"shared_expense_id"` // echo of a shared-pocket expense
	SettlementID  *int64  `json:"settlement_id"`     // echo of a shared-pocket settlement
	CreatedAt     string  `json:"created_at"`
}

// resolvePocketID applies the tri-state pocket contract for txn creation:
// field absent/null → the user's active pocket (old clients get the default
// for free); 0 → explicit General (stored NULL); N → that pocket, validated
// as owned + personal + not archived. Returns (stored value, error message).
func resolvePocketID(d *db.DB, uid string, requested *int64) (*int64, string) {
	if requested == nil {
		return activePocketID(d, uid), ""
	}
	if *requested == 0 {
		return nil, ""
	}
	var ok bool
	d.QueryRow(`SELECT NOT archived FROM fin_pockets WHERE id = $1 AND user_id = $2 AND kind = 'personal'`,
		*requested, uid).Scan(&ok)
	if !ok {
		return nil, "pocket not found"
	}
	return requested, ""
}

// istDateExpr is the SQL that projects a txn_at TIMESTAMPTZ down to its IST
// calendar date — used wherever a query filters/groups by day, so the existing
// date-string params (from/to, month boundaries) keep their exact semantics
// after the txn_date→txn_at migration.
const istDateExpr = "(%s AT TIME ZONE 'Asia/Kolkata')::date"

func istDate(col string) string { return fmt.Sprintf(istDateExpr, col) }

// resolveTxnAt parses the request's ISO txn_at, falling back to `now` when
// absent or unparseable. The legacy date-only txn_date shim is gone: both
// clients (web, android) send txn_at.
func resolveTxnAt(txnAt string, now time.Time) time.Time {
	if txnAt != "" {
		if t, err := time.Parse(time.RFC3339, txnAt); err == nil {
			return t
		}
	}
	return now
}

// composeTxnAt builds an IST instant from a date ("YYYY-MM-DD") and an optional
// time ("HH:MM"). A blank/invalid time falls back to now's clock, so an
// AI-parsed message that states no time lands on the current minute rather than
// midnight (per the parse-message contract). Bad date → now.
func composeTxnAt(loc *time.Location, date, hhmm string, now time.Time) time.Time {
	d, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return now
	}
	hh, mm, ss := now.Hour(), now.Minute(), now.Second()
	if t, err := time.ParseInLocation("15:04", hhmm, loc); err == nil {
		hh, mm, ss = t.Hour(), t.Minute(), 0
	}
	return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, ss, 0, loc)
}

func listTransactions(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())

		args := []any{uid}
		clauses := []string{"t.user_id = $1"}
		ph := 2
		if v := queryParam(r, "account_id"); v != "" {
			clauses = append(clauses, "t.account_id = $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if v := queryParam(r, "category_id"); v != "" {
			clauses = append(clauses, "t.category_id = $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if v := queryParam(r, "type"); v != "" {
			clauses = append(clauses, "t.type = $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if v := queryParam(r, "from"); v != "" {
			clauses = append(clauses, istDate("t.txn_at")+" >= $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if v := queryParam(r, "to"); v != "" {
			clauses = append(clauses, istDate("t.txn_at")+" <= $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if v := queryParam(r, "search"); v != "" {
			clauses = append(clauses, "t.description ILIKE $"+itoa(ph))
			args = append(args, "%"+v+"%")
			ph++
		}
		// pocket_id=N filters to that pocket; pocket_id=0 = General (NULL).
		if v := queryParam(r, "pocket_id"); v != "" {
			if v == "0" {
				clauses = append(clauses, "t.pocket_id IS NULL")
			} else {
				clauses = append(clauses, "t.pocket_id = $"+itoa(ph))
				args = append(args, v)
				ph++
			}
		}

		limit := 200
		if v := queryParam(r, "limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}

		q := `SELECT t.id, t.account_id, a.name, t.category_id, c.name, c.color, t.type, t.amount,
			  t.description, t.note, t.txn_at, t.transfer_pair, t.linked_account, t.pocket_id, p.name,
			  t.shared_expense_id, t.settlement_id, t.created_at::text
			  FROM fin_transactions t
			  JOIN fin_accounts a ON a.id = t.account_id
			  LEFT JOIN fin_categories c ON c.id = t.category_id
			  LEFT JOIN fin_pockets p ON p.id = t.pocket_id
			  WHERE ` + strings.Join(clauses, " AND ") +
			` ORDER BY t.txn_at DESC, t.id DESC LIMIT ` + itoa(limit)

		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		loc := userLocation(d, uid)
		var out []txnResp
		for rows.Next() {
			var t txnResp
			var at time.Time
			rows.Scan(&t.ID, &t.AccountID, &t.AccountName, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
				&t.Type, &t.Amount, &t.Description, &t.Note, &at, &t.TransferPair, &t.LinkedAccount, &t.PocketID, &t.PocketName,
				&t.SharedExpense, &t.SettlementID, &t.CreatedAt)
			t.TxnAt = at.In(loc).Format(time.RFC3339)
			out = append(out, t)
		}
		if out == nil {
			out = []txnResp{}
		}
		writeJSON(w, 200, out)
	}
}

func createTransaction(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			AccountID     int64   `json:"account_id"`
			CategoryID    *int64  `json:"category_id"`
			Type          string  `json:"type"`
			Amount        float64 `json:"amount"`
			Description   string  `json:"description"`
			Note          string  `json:"note"`
			TxnAt         string  `json:"txn_at"` // RFC3339 (IST)
			LinkedAccount *int64  `json:"linked_account"`
			PocketID      *int64  `json:"pocket_id"` // absent → active pocket; 0 → General; N → pocket
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.AccountID == 0 {
			errJSON(w, 400, "account_id required")
			return
		}
		ctx := r.Context()
		refs := []struct {
			table string
			id    *int64
		}{
			{"fin_accounts", &b.AccountID},
			{"fin_categories", b.CategoryID},
			{"fin_accounts", b.LinkedAccount},
		}
		if b.PocketID != nil && *b.PocketID != 0 {
			refs = append(refs, struct {
				table string
				id    *int64
			}{"fin_pockets", b.PocketID})
		}
		for _, ref := range refs {
			if ref.id == nil {
				continue
			}
			if err := requireOwnedFinanceRef(ctx, d, ref.table, uid, *ref.id); err != nil {
				if errors.Is(err, errFinanceReferenceNotFound) {
					errJSON(w, 404, "not found")
				} else {
					internalError(w, r, "validate transaction reference", err)
				}
				return
			}
		}
		if b.Type == "" {
			b.Type = "expense"
		}
		pocketID, perrMsg := resolvePocketID(d, uid, b.PocketID)
		if perrMsg != "" {
			errJSON(w, 400, perrMsg)
			return
		}
		txnAt := resolveTxnAt(b.TxnAt, userNow(d, uid))

		// Transfer: insert a pair (transfer_out from source, transfer_in to dest)
		if b.Type == "transfer" {
			if b.LinkedAccount == nil || *b.LinkedAccount == b.AccountID {
				errJSON(w, 400, "transfer requires distinct linked_account")
				return
			}
			tx, err := d.Begin()
			if err != nil {
				internalError(w, r, "begin transfer", err)
				return
			}
			var outID, inID int64
			if err := tx.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, note, txn_at, linked_account) VALUES ($1,$2,'transfer_out',$3,$4,$5,$6,$7) RETURNING id`,
				uid, b.AccountID, b.Amount, b.Description, b.Note, txnAt, *b.LinkedAccount).Scan(&outID); err != nil {
				tx.Rollback()
				internalError(w, r, "insert transfer debit", err)
				return
			}
			if err := tx.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, note, txn_at, linked_account, transfer_pair) VALUES ($1,$2,'transfer_in',$3,$4,$5,$6,$7,$8) RETURNING id`,
				uid, *b.LinkedAccount, b.Amount, b.Description, b.Note, txnAt, b.AccountID, outID).Scan(&inID); err != nil {
				tx.Rollback()
				internalError(w, r, "insert transfer credit", err)
				return
			}
			if _, err := tx.Exec("UPDATE fin_transactions SET transfer_pair = $1 WHERE id = $2", inID, outID); err != nil {
				tx.Rollback()
				internalError(w, r, "link transfer pair", err)
				return
			}
			if err := tx.Commit(); err != nil {
				internalError(w, r, "commit transfer", err)
				return
			}
			// Index #hashtags from the note (the out leg carries the txn id).
			syncTags(d, uid, "transaction", outID, b.Note)
			writeJSON(w, 201, map[string]int64{"id": outID, "pair_id": inID})
			return
		}

		var id int64
		err := d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, note, txn_at, pocket_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
			uid, b.AccountID, b.CategoryID, b.Type, b.Amount, b.Description, b.Note, txnAt, pocketID,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		// Index #hashtags from the note into the shared tag system.
		syncTags(d, uid, "transaction", id, b.Note)
		// Learn merchant → category so the next shared SMS for the same
		// merchant pre-fills without an AI call.
		learnMerchantCategory(d, uid, b.Description, b.CategoryID)
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateTransaction(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			AccountID   *int64   `json:"account_id"`
			CategoryID  *int64   `json:"category_id"`
			Amount      *float64 `json:"amount"`
			Description *string  `json:"description"`
			Note        *string  `json:"note"`
			TxnAt       *string  `json:"txn_at"`    // RFC3339 (IST)
			PocketID    *int64   `json:"pocket_id"` // 0 → General; N → pocket; absent → untouched
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin transaction edit", err)
			return
		}
		defer tx.Rollback()
		if err := requireOwnedFinanceRef(ctx, tx, "fin_transactions", uid, id); err != nil {
			if errors.Is(err, errFinanceReferenceNotFound) {
				errJSON(w, 404, "not found")
			} else {
				internalError(w, r, "find transaction", err)
			}
			return
		}
		for _, ref := range []struct {
			table string
			id    *int64
		}{{"fin_accounts", b.AccountID}, {"fin_categories", b.CategoryID}} {
			if ref.id != nil {
				if err := requireOwnedFinanceRef(ctx, tx, ref.table, uid, *ref.id); err != nil {
					if errors.Is(err, errFinanceReferenceNotFound) {
						errJSON(w, 404, "not found")
					} else {
						internalError(w, r, "validate transaction edit", err)
					}
					return
				}
			}
		}
		if b.PocketID != nil && *b.PocketID != 0 {
			if err := requirePersonalPocket(ctx, tx, uid, *b.PocketID); err != nil {
				if errors.Is(err, errFinanceReferenceNotFound) {
					errJSON(w, 404, "not found")
				} else {
					internalError(w, r, "validate transaction pocket", err)
				}
				return
			}
		}

		// Echo rows mirror a shared-pocket expense/settlement: their money
		// fields are managed by the pocket, only account/category/note are
		// the user's to edit here.
		var echoLinked bool
		if err := tx.QueryRowContext(ctx, `SELECT shared_expense_id IS NOT NULL OR settlement_id IS NOT NULL
			FROM fin_transactions WHERE id = $1`, id).Scan(&echoLinked); err != nil {
			internalError(w, r, "check echo link", err)
			return
		}
		if echoLinked && (b.Amount != nil || b.Description != nil || b.TxnAt != nil || b.PocketID != nil) {
			errJSON(w, 400, "amount, description and date are managed by the shared pocket")
			return
		}

		var mainSets, pairSets []string
		var mainArgs, pairArgs []any
		add := func(sets *[]string, args *[]any, column string, value any) {
			*args = append(*args, value)
			*sets = append(*sets, fmt.Sprintf("%s=$%d", column, len(*args)))
		}
		if b.PocketID != nil {
			var value any
			if *b.PocketID != 0 {
				value = *b.PocketID
			}
			add(&mainSets, &mainArgs, "pocket_id", value)
		}
		if b.AccountID != nil {
			add(&mainSets, &mainArgs, "account_id", *b.AccountID)
			add(&pairSets, &pairArgs, "linked_account", *b.AccountID)
		}
		if b.CategoryID != nil {
			add(&mainSets, &mainArgs, "category_id", *b.CategoryID)
		}
		for _, value := range []struct {
			column string
			value  any
			set    bool
		}{
			{"amount", valueOrNil(b.Amount), b.Amount != nil},
			{"description", valueOrNil(b.Description), b.Description != nil},
			{"note", valueOrNil(b.Note), b.Note != nil},
		} {
			if value.set {
				add(&mainSets, &mainArgs, value.column, value.value)
				add(&pairSets, &pairArgs, value.column, value.value)
			}
		}
		if b.TxnAt != nil {
			value := resolveTxnAt(*b.TxnAt, userNow(d, uid))
			add(&mainSets, &mainArgs, "txn_at", value)
			add(&pairSets, &pairArgs, "txn_at", value)
		}
		if len(mainSets) > 0 {
			mainArgs = append(mainArgs, id, uid)
			query := fmt.Sprintf("UPDATE fin_transactions SET %s, updated_at=NOW() WHERE id=$%d AND user_id=$%d", strings.Join(mainSets, ","), len(mainArgs)-1, len(mainArgs))
			if _, err := tx.ExecContext(ctx, query, mainArgs...); err != nil {
				internalError(w, r, "update transaction", fmt.Errorf("update main row: %w", err))
				return
			}
		}
		if len(pairSets) > 0 {
			pairArgs = append(pairArgs, id, uid)
			query := fmt.Sprintf("UPDATE fin_transactions SET %s, updated_at=NOW() WHERE transfer_pair=$%d AND user_id=$%d", strings.Join(pairSets, ","), len(pairArgs)-1, len(pairArgs))
			if _, err := tx.ExecContext(ctx, query, pairArgs...); err != nil {
				internalError(w, r, "update transaction pair", fmt.Errorf("update paired row: %w", err))
				return
			}
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit transaction edit", err)
			return
		}
		if b.Note != nil {
			syncTags(d, uid, "transaction", id, *b.Note)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func valueOrNil[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}

func deleteTransaction(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		// Echo rows die with their shared expense/settlement, not here.
		var echoLinked bool
		d.QueryRow(`SELECT shared_expense_id IS NOT NULL OR settlement_id IS NOT NULL
			FROM fin_transactions WHERE id = $1 AND user_id = $2`, id, uid).Scan(&echoLinked)
		if echoLinked {
			errJSON(w, 400, "this entry is managed by a shared pocket — delete it there")
			return
		}
		// delete pair if any
		var pair *int64
		d.QueryRow("SELECT transfer_pair FROM fin_transactions WHERE id = $1 AND user_id = $2", id, uid).Scan(&pair)
		d.Exec("DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2", id, uid)
		d.Exec("DELETE FROM tags WHERE user_id = $1 AND entity_type = 'transaction' AND entity_id = $2", uid, id)
		if pair != nil {
			d.Exec("DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2", *pair, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- budgets ---------------------------------------------------------------

type budgetItemResp struct {
	ID         int64   `json:"id"`
	CategoryID *int64  `json:"category_id"`
	Amount     float64 `json:"amount"`
	Spent      float64 `json:"spent"`
}
type budgetResp struct {
	ID          int64            `json:"id"`
	Name        string           `json:"name"`
	Period      string           `json:"period"`
	StartDate   string           `json:"start_date"`
	EndDate     string           `json:"end_date"`
	WindowStart string           `json:"window_start"`
	WindowEnd   string           `json:"window_end"`
	TotalAmount float64          `json:"total_amount"`
	Spent       float64          `json:"spent"`
	PocketIDs   []int64          `json:"pocket_ids"`
	Items       []budgetItemResp `json:"items"`
}

// budgetWindow resolves the date window spend is computed over. Monthly
// budgets auto-roll: they ignore their stored dates and use the requested
// IST month (monthParam "YYYY-MM", default = the current month). Custom
// budgets use their stored range.
func budgetWindow(period, startDate, endDate, monthParam string, now time.Time) (string, string) {
	if period != "monthly" {
		return startDate, endDate
	}
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if monthParam != "" {
		if m, err := time.Parse("2006-01", monthParam); err == nil {
			first = m
		}
	}
	last := first.AddDate(0, 1, -1)
	return first.Format("2006-01-02"), last.Format("2006-01-02")
}

// budgetSpent computes the overall spend in a window, optionally restricted
// to a pocket set. General (NULL pocket) txns never match a pocket filter.
func budgetSpent(d *db.DB, uid, from, to string, pocketIDs []int64) float64 {
	var spent float64
	if len(pocketIDs) > 0 {
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND type = 'expense' AND pocket_id = ANY($2)
			AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $3 AND $4`,
			uid, pocketIDs, from, to).Scan(&spent)
	} else {
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND type = 'expense'
			AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3`,
			uid, from, to).Scan(&spent)
	}
	return spent
}

// categorySpent computes a category cap's spend. A cap is a slice of its
// budget, so it reads through the SAME pocket lens as the overall spent —
// a trip budget's "Food" cap must not absorb General food spends that merely
// share the date range. Mirrored in ai.listBudgetsTool and exportBudgetsCSV.
func categorySpent(d *db.DB, uid string, catID int64, from, to string, pocketIDs []int64) float64 {
	var spent float64
	if len(pocketIDs) > 0 {
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND type = 'expense' AND category_id = $2 AND pocket_id = ANY($3)
			AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $4 AND $5`,
			uid, catID, pocketIDs, from, to).Scan(&spent)
	} else {
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND type = 'expense' AND category_id = $2
			AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $3 AND $4`,
			uid, catID, from, to).Scan(&spent)
	}
	return spent
}

func listBudgets(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		month := queryParam(r, "month") // YYYY-MM; monthly budgets only
		now := userNow(d, uid)
		rows, err := d.Query(`SELECT id, name, period, start_date::text, end_date::text, total_amount
			FROM fin_budgets WHERE user_id = $1 ORDER BY start_date DESC`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()

		var out []budgetResp
		for rows.Next() {
			var b budgetResp
			rows.Scan(&b.ID, &b.Name, &b.Period, &b.StartDate, &b.EndDate, &b.TotalAmount)
			b.WindowStart, b.WindowEnd = budgetWindow(b.Period, b.StartDate, b.EndDate, month, now)
			// pocket filter
			b.PocketIDs = []int64{}
			prows, _ := d.Query(`SELECT pocket_id FROM fin_budget_pockets WHERE budget_id = $1`, b.ID)
			if prows != nil {
				for prows.Next() {
					var pid int64
					prows.Scan(&pid)
					b.PocketIDs = append(b.PocketIDs, pid)
				}
				prows.Close()
			}
			// items (category caps — inherit the budget's pocket filter)
			itemRows, _ := d.Query(`SELECT id, category_id, amount FROM fin_budget_items WHERE budget_id = $1`, b.ID)
			if itemRows != nil {
				for itemRows.Next() {
					var it budgetItemResp
					itemRows.Scan(&it.ID, &it.CategoryID, &it.Amount)
					if it.CategoryID != nil {
						it.Spent = categorySpent(d, uid, *it.CategoryID, b.WindowStart, b.WindowEnd, b.PocketIDs)
					}
					b.Items = append(b.Items, it)
				}
				itemRows.Close()
			}
			if b.Items == nil {
				b.Items = []budgetItemResp{}
			}
			b.Spent = budgetSpent(d, uid, b.WindowStart, b.WindowEnd, b.PocketIDs)
			out = append(out, b)
		}
		if out == nil {
			out = []budgetResp{}
		}
		writeJSON(w, 200, out)
	}
}

func createBudget(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			Name        string  `json:"name"`
			Period      string  `json:"period"`
			StartDate   string  `json:"start_date"`
			EndDate     string  `json:"end_date"`
			TotalAmount float64 `json:"total_amount"`
			PocketIDs   []int64 `json:"pocket_ids"`
			Items       []struct {
				CategoryID *int64  `json:"category_id"`
				Amount     float64 `json:"amount"`
			} `json:"items"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Period == "" {
			b.Period = "monthly"
		}
		// Monthly budgets auto-roll — the stored dates are display-inert but
		// kept populated (current month) for legacy readers.
		if b.Period == "monthly" && (b.StartDate == "" || b.EndDate == "") {
			b.StartDate, b.EndDate = budgetWindow("monthly", "", "", "", userNow(d, uid))
		}
		ctx := r.Context()
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin budget create", err)
			return
		}
		defer tx.Rollback()
		for _, item := range b.Items {
			if item.CategoryID != nil {
				if err := requireOwnedFinanceRef(ctx, tx, "fin_categories", uid, *item.CategoryID); err != nil {
					errJSON(w, 404, "not found")
					return
				}
			}
		}
		for _, pocketID := range b.PocketIDs {
			if err := requirePersonalPocket(ctx, tx, uid, pocketID); err != nil {
				errJSON(w, 404, "not found")
				return
			}
		}
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO fin_budgets (user_id, name, period, start_date, end_date, total_amount) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			uid, b.Name, b.Period, b.StartDate, b.EndDate, b.TotalAmount,
		).Scan(&id)
		if err != nil {
			internalError(w, r, "insert budget", err)
			return
		}
		for _, it := range b.Items {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fin_budget_items (budget_id, user_id, category_id, amount) VALUES ($1,$2,$3,$4)`, id, uid, it.CategoryID, it.Amount); err != nil {
				internalError(w, r, "insert budget item", err)
				return
			}
		}
		for _, pid := range b.PocketIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fin_budget_pockets (budget_id, user_id, pocket_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, id, uid, pid); err != nil {
				internalError(w, r, "insert budget pocket", err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit budget create", err)
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateBudget(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			Name        *string  `json:"name"`
			Period      *string  `json:"period"`
			StartDate   *string  `json:"start_date"`
			EndDate     *string  `json:"end_date"`
			TotalAmount *float64 `json:"total_amount"`
			PocketIDs   *[]int64 `json:"pocket_ids"`
			Items       *[]struct {
				CategoryID *int64  `json:"category_id"`
				Amount     float64 `json:"amount"`
			} `json:"items"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin budget edit", err)
			return
		}
		defer tx.Rollback()
		if err := requireOwnedFinanceRef(ctx, tx, "fin_budgets", uid, id); err != nil {
			errJSON(w, 404, "not found")
			return
		}
		for _, item := range valueOrEmpty(b.Items) {
			if item.CategoryID != nil {
				if err := requireOwnedFinanceRef(ctx, tx, "fin_categories", uid, *item.CategoryID); err != nil {
					errJSON(w, 404, "not found")
					return
				}
			}
		}
		for _, pocketID := range valueOrEmpty(b.PocketIDs) {
			if err := requirePersonalPocket(ctx, tx, uid, pocketID); err != nil {
				errJSON(w, 404, "not found")
				return
			}
		}
		for _, update := range []struct {
			query string
			value any
			set   bool
		}{
			{"UPDATE fin_budgets SET name=$1 WHERE id=$2 AND user_id=$3", valueOrNil(b.Name), b.Name != nil},
			{"UPDATE fin_budgets SET period=$1 WHERE id=$2 AND user_id=$3", valueOrNil(b.Period), b.Period != nil},
			{"UPDATE fin_budgets SET start_date=$1 WHERE id=$2 AND user_id=$3", valueOrNil(b.StartDate), b.StartDate != nil},
			{"UPDATE fin_budgets SET end_date=$1 WHERE id=$2 AND user_id=$3", valueOrNil(b.EndDate), b.EndDate != nil},
			{"UPDATE fin_budgets SET total_amount=$1 WHERE id=$2 AND user_id=$3", valueOrNil(b.TotalAmount), b.TotalAmount != nil},
		} {
			if update.set {
				if _, err := tx.ExecContext(ctx, update.query, update.value, id, uid); err != nil {
					internalError(w, r, "update budget", err)
					return
				}
			}
		}
		if b.Items != nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM fin_budget_items WHERE budget_id=$1 AND user_id=$2", id, uid); err != nil {
				internalError(w, r, "replace budget items", err)
				return
			}
			for _, it := range *b.Items {
				if _, err := tx.ExecContext(ctx, "INSERT INTO fin_budget_items (budget_id,user_id,category_id,amount) VALUES ($1,$2,$3,$4)", id, uid, it.CategoryID, it.Amount); err != nil {
					internalError(w, r, "replace budget item", err)
					return
				}
			}
		}
		if b.PocketIDs != nil {
			if _, err := tx.ExecContext(ctx, "DELETE FROM fin_budget_pockets WHERE budget_id=$1 AND user_id=$2", id, uid); err != nil {
				internalError(w, r, "replace budget pockets", err)
				return
			}
			for _, pid := range *b.PocketIDs {
				if _, err := tx.ExecContext(ctx, "INSERT INTO fin_budget_pockets (budget_id,user_id,pocket_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING", id, uid, pid); err != nil {
					internalError(w, r, "replace budget pocket", err)
					return
				}
			}
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit budget edit", err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func valueOrEmpty[T any](value *[]T) []T {
	if value == nil {
		return nil
	}
	return *value
}

func deleteBudget(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM fin_budgets WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- investments -----------------------------------------------------------

func listInvestments(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`SELECT id, name, type, account_id, invested_amount, current_value, monthly_amount,
			frequency, start_date::text, maturity_date::text, expected_return, notes, last_updated::text,
			auto_debit, next_debit_date::text
			FROM fin_investments WHERE user_id = $1 ORDER BY created_at ASC`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Inv struct {
			ID             int64   `json:"id"`
			Name           string  `json:"name"`
			Type           string  `json:"type"`
			AccountID      *int64  `json:"account_id"`
			InvestedAmount float64 `json:"invested_amount"`
			CurrentValue   float64 `json:"current_value"`
			MonthlyAmount  float64 `json:"monthly_amount"`
			Frequency      string  `json:"frequency"`
			StartDate      *string `json:"start_date"`
			MaturityDate   *string `json:"maturity_date"`
			ExpectedReturn float64 `json:"expected_return"`
			Notes          string  `json:"notes"`
			LastUpdated    string  `json:"last_updated"`
			AutoDebit      bool    `json:"auto_debit"`
			NextDebitDate  *string `json:"next_debit_date"`
		}
		var out []Inv
		for rows.Next() {
			var i Inv
			rows.Scan(&i.ID, &i.Name, &i.Type, &i.AccountID, &i.InvestedAmount, &i.CurrentValue, &i.MonthlyAmount,
				&i.Frequency, &i.StartDate, &i.MaturityDate, &i.ExpectedReturn, &i.Notes, &i.LastUpdated,
				&i.AutoDebit, &i.NextDebitDate)
			out = append(out, i)
		}
		if out == nil {
			out = []Inv{}
		}
		writeJSON(w, 200, out)
	}
}

// validInvestmentType gates the manual instrument set (market trading was
// removed from the product; sip/mutual_fund live on as manual entries).
func validInvestmentType(t string) bool {
	switch t {
	case "sip", "mutual_fund", "fd", "rd", "other":
		return true
	}
	return false
}

// validateAutoDebit enforces what the auto-debit cron needs: an account to
// debit, a per-cycle amount, and a recurring frequency.
func validateAutoDebit(accountID *int64, monthlyAmount float64, frequency string) string {
	if accountID == nil {
		return "auto-debit needs a linked account"
	}
	if monthlyAmount <= 0 {
		return "auto-debit needs a per-cycle amount"
	}
	switch frequency {
	case "monthly", "quarterly", "yearly":
		return ""
	}
	return "auto-debit needs a recurring frequency (monthly/quarterly/yearly)"
}

func createInvestment(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			Name           string  `json:"name"`
			Type           string  `json:"type"`
			AccountID      *int64  `json:"account_id"`
			InvestedAmount float64 `json:"invested_amount"`
			CurrentValue   float64 `json:"current_value"`
			MonthlyAmount  float64 `json:"monthly_amount"`
			Frequency      string  `json:"frequency"`
			StartDate      *string `json:"start_date"`
			MaturityDate   *string `json:"maturity_date"`
			ExpectedReturn float64 `json:"expected_return"`
			Notes          string  `json:"notes"`
			AutoDebit      bool    `json:"auto_debit"`
			NextDebitDate  *string `json:"next_debit_date"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Type == "" {
			b.Type = "sip"
		}
		if !validInvestmentType(b.Type) {
			errJSON(w, 400, "invalid investment type")
			return
		}
		// Recurring contributions (sip/rd) default monthly; the rest one-off.
		if b.Frequency == "" {
			switch b.Type {
			case "sip", "rd":
				b.Frequency = "monthly"
			default:
				b.Frequency = "lumpsum"
			}
		}
		var nextDebit *string
		if b.AutoDebit {
			if msg := validateAutoDebit(b.AccountID, b.MonthlyAmount, b.Frequency); msg != "" {
				errJSON(w, 400, msg)
				return
			}
			nextDebit = b.NextDebitDate
			if nextDebit == nil || *nextDebit == "" {
				nd := defaultNextDebitDate(b.StartDate, userNow(d, uid))
				nextDebit = &nd
			}
		}
		if b.CurrentValue == 0 {
			b.CurrentValue = b.InvestedAmount
		}
		if b.AccountID != nil {
			if err := requireOwnedFinanceRef(r.Context(), d, "fin_accounts", uid, *b.AccountID); err != nil {
				errJSON(w, 404, "not found")
				return
			}
		}
		var anchorDay any
		if nextDebit != nil && *nextDebit != "" {
			next, err := time.Parse("2006-01-02", *nextDebit)
			if err != nil {
				errJSON(w, 400, "invalid next_debit_date")
				return
			}
			anchorDay = next.Day()
		}

		var id int64
		err := d.QueryRow(`INSERT INTO fin_investments
			(user_id, name, type, account_id, invested_amount, current_value, monthly_amount, frequency, start_date, maturity_date, expected_return, notes, auto_debit, next_debit_date, anchor_day)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
			uid, b.Name, b.Type, b.AccountID, b.InvestedAmount, b.CurrentValue, b.MonthlyAmount, b.Frequency,
			b.StartDate, b.MaturityDate, b.ExpectedReturn, b.Notes, b.AutoDebit, nextDebit, anchorDay,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateInvestment(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			Name           *string  `json:"name"`
			Type           *string  `json:"type"`
			AccountID      *int64   `json:"account_id"`
			InvestedAmount *float64 `json:"invested_amount"`
			CurrentValue   *float64 `json:"current_value"`
			MonthlyAmount  *float64 `json:"monthly_amount"`
			Frequency      *string  `json:"frequency"`
			StartDate      *string  `json:"start_date"`
			MaturityDate   *string  `json:"maturity_date"`
			ExpectedReturn *float64 `json:"expected_return"`
			Notes          *string  `json:"notes"`
			AutoDebit      *bool    `json:"auto_debit"`
			NextDebitDate  *string  `json:"next_debit_date"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Type != nil && !validInvestmentType(*b.Type) {
			errJSON(w, 400, "invalid investment type")
			return
		}
		if b.AccountID != nil {
			if err := requireOwnedFinanceRef(r.Context(), d, "fin_accounts", uid, *b.AccountID); err != nil {
				errJSON(w, 404, "not found")
				return
			}
		}
		// Enabling auto-debit re-validates against the row's effective state
		// (patch value when sent, stored value otherwise).
		if b.AutoDebit != nil && *b.AutoDebit {
			var curAcct *int64
			var curMonthly float64
			var curFreq string
			var curStart *string
			if err := d.QueryRow(`SELECT account_id, monthly_amount, frequency, start_date::text
				FROM fin_investments WHERE id = $1 AND user_id = $2`, id, uid).Scan(&curAcct, &curMonthly, &curFreq, &curStart); err != nil {
				errJSON(w, 404, "not found")
				return
			}
			acct, monthly, freq := curAcct, curMonthly, curFreq
			if b.AccountID != nil {
				acct = b.AccountID
			}
			if b.MonthlyAmount != nil {
				monthly = *b.MonthlyAmount
			}
			if b.Frequency != nil {
				freq = *b.Frequency
			}
			if msg := validateAutoDebit(acct, monthly, freq); msg != "" {
				errJSON(w, 400, msg)
				return
			}
			if b.NextDebitDate == nil || *b.NextDebitDate == "" {
				start := curStart
				if b.StartDate != nil {
					start = b.StartDate
				}
				nd := defaultNextDebitDate(start, userNow(d, uid))
				b.NextDebitDate = &nd
			}
		}
		set := []string{"last_updated = NOW()"}
		args := []any{}
		ph := 1
		add := func(col string, v any) {
			set = append(set, col+" = $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if b.Name != nil {
			add("name", *b.Name)
		}
		if b.Type != nil {
			add("type", *b.Type)
		}
		if b.AccountID != nil {
			add("account_id", *b.AccountID)
		}
		if b.InvestedAmount != nil {
			add("invested_amount", *b.InvestedAmount)
		}
		if b.CurrentValue != nil {
			add("current_value", *b.CurrentValue)
		}
		if b.MonthlyAmount != nil {
			add("monthly_amount", *b.MonthlyAmount)
		}
		if b.Frequency != nil {
			add("frequency", *b.Frequency)
		}
		if b.StartDate != nil {
			add("start_date", *b.StartDate)
		}
		if b.MaturityDate != nil {
			add("maturity_date", *b.MaturityDate)
		}
		if b.ExpectedReturn != nil {
			add("expected_return", *b.ExpectedReturn)
		}
		if b.Notes != nil {
			add("notes", *b.Notes)
		}
		if b.AutoDebit != nil {
			add("auto_debit", *b.AutoDebit)
			if !*b.AutoDebit {
				set = append(set, "next_debit_date = NULL")
			}
		}
		if b.NextDebitDate != nil && *b.NextDebitDate != "" {
			next, err := time.Parse("2006-01-02", *b.NextDebitDate)
			if err != nil {
				errJSON(w, 400, "invalid next_debit_date")
				return
			}
			add("next_debit_date", *b.NextDebitDate)
			add("anchor_day", next.Day())
		}
		args = append(args, id, uid)
		q := "UPDATE fin_investments SET " + strings.Join(set, ", ") + " WHERE id = $" + itoa(ph) + " AND user_id = $" + itoa(ph+1)
		if _, err := d.Exec(q, args...); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteInvestment(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM fin_investments WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- virtual savings -------------------------------------------------------

func listSavings(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		accountID := queryParam(r, "account_id")
		args := []any{uid}
		q := `SELECT s.id, s.account_id, a.name, s.name, s.target_amount, s.current_amount, s.color, s.created_at::text
			FROM fin_virtual_savings s JOIN fin_accounts a ON a.id = s.account_id
			WHERE s.user_id = $1`
		if accountID != "" {
			q += " AND s.account_id = $2"
			args = append(args, accountID)
		}
		q += " ORDER BY s.created_at ASC"
		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Saving struct {
			ID            int64   `json:"id"`
			AccountID     int64   `json:"account_id"`
			AccountName   string  `json:"account_name"`
			Name          string  `json:"name"`
			TargetAmount  float64 `json:"target_amount"`
			CurrentAmount float64 `json:"current_amount"`
			Color         string  `json:"color"`
			CreatedAt     string  `json:"created_at"`
		}
		var out []Saving
		for rows.Next() {
			var s Saving
			rows.Scan(&s.ID, &s.AccountID, &s.AccountName, &s.Name, &s.TargetAmount, &s.CurrentAmount, &s.Color, &s.CreatedAt)
			out = append(out, s)
		}
		if out == nil {
			out = []Saving{}
		}
		writeJSON(w, 200, out)
	}
}

func createSaving(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var b struct {
			AccountID     int64   `json:"account_id"`
			Name          string  `json:"name"`
			TargetAmount  float64 `json:"target_amount"`
			CurrentAmount float64 `json:"current_amount"`
			Color         string  `json:"color"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Color == "" {
			b.Color = "#2D5A4F"
		}
		var id int64
		err := d.QueryRow(`INSERT INTO fin_virtual_savings (user_id, account_id, name, target_amount, current_amount, color)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			uid, b.AccountID, b.Name, b.TargetAmount, b.CurrentAmount, b.Color,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateSaving(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			Name          *string  `json:"name"`
			TargetAmount  *float64 `json:"target_amount"`
			CurrentAmount *float64 `json:"current_amount"`
			Color         *string  `json:"color"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Name != nil {
			d.Exec("UPDATE fin_virtual_savings SET name = $1 WHERE id = $2 AND user_id = $3", *b.Name, id, uid)
		}
		if b.TargetAmount != nil {
			d.Exec("UPDATE fin_virtual_savings SET target_amount = $1 WHERE id = $2 AND user_id = $3", *b.TargetAmount, id, uid)
		}
		if b.CurrentAmount != nil {
			d.Exec("UPDATE fin_virtual_savings SET current_amount = $1 WHERE id = $2 AND user_id = $3", *b.CurrentAmount, id, uid)
		}
		if b.Color != nil {
			d.Exec("UPDATE fin_virtual_savings SET color = $1 WHERE id = $2 AND user_id = $3", *b.Color, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteSaving(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM fin_virtual_savings WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- credit card statements ------------------------------------------------

func listStatements(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		args := []any{uid}
		q := `SELECT s.id, s.account_id, a.name, s.statement_date::text, s.due_date::text,
			s.amount_due, s.new_charges, s.previous_balance, s.cashback_earned, s.paid, s.paid_at::text
			FROM fin_cc_statements s JOIN fin_accounts a ON a.id = s.account_id
			WHERE s.user_id = $1`
		if v := queryParam(r, "account_id"); v != "" {
			q += " AND s.account_id = $2"
			args = append(args, v)
		}
		q += " ORDER BY s.due_date DESC"
		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Stmt struct {
			ID              int64   `json:"id"`
			AccountID       int64   `json:"account_id"`
			AccountName     string  `json:"account_name"`
			StatementDate   string  `json:"statement_date"`
			DueDate         string  `json:"due_date"`
			AmountDue       float64 `json:"amount_due"`
			NewCharges      float64 `json:"new_charges"`
			PreviousBalance float64 `json:"previous_balance"`
			CashbackEarned  float64 `json:"cashback_earned"`
			Paid            bool    `json:"paid"`
			PaidAt          *string `json:"paid_at"`
		}
		var out []Stmt
		for rows.Next() {
			var s Stmt
			rows.Scan(&s.ID, &s.AccountID, &s.AccountName, &s.StatementDate, &s.DueDate,
				&s.AmountDue, &s.NewCharges, &s.PreviousBalance, &s.CashbackEarned, &s.Paid, &s.PaidAt)
			out = append(out, s)
		}
		if out == nil {
			out = []Stmt{}
		}
		writeJSON(w, 200, out)
	}
}

// deriveDueDate computes a statement's due date from the card's due_day: the
// due day in the same month when it falls after the statement day, otherwise
// the next month. Clamped to the 28th to avoid month-length overflow.
func deriveDueDate(deps Deps, uid string, acctID int64, stmtDate string) string {
	d := deps.DB
	t, err := time.Parse("2006-01-02", stmtDate)
	if err != nil {
		return stmtDate
	}
	var dueDay *int
	d.QueryRow("SELECT due_day FROM fin_accounts WHERE id = $1 AND user_id = $2", acctID, uid).Scan(&dueDay)
	dd := t.Day() + 15
	if dueDay != nil && *dueDay > 0 {
		dd = *dueDay
	}
	year, month := t.Year(), t.Month()
	if dd <= t.Day() {
		month++
		if month > 12 {
			month = 1
			year++
		}
	}
	if dd > 28 {
		dd = 28
	}
	return time.Date(year, month, dd, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

type statementCalcInput struct {
	StatementDate  string
	AmountDue      *float64
	NewCharges     *float64
	CashbackEarned *float64
}

type statementCalcResult struct {
	AmountDue       float64 `json:"amount_due"`
	NewCharges      float64 `json:"new_charges"`
	PreviousBalance float64 `json:"previous_balance"`
	CashbackEarned  float64 `json:"cashback_earned"`
	Payments        float64 `json:"payments"`
}

func computeStatementTotals(d *db.DB, uid string, acctID int64, in statementCalcInput) statementCalcResult {
	// Cycle window: previous statement_date (exclusive) -> this one (inclusive).
	var lastStmt *string
	d.QueryRow("SELECT MAX(statement_date)::text FROM fin_cc_statements WHERE user_id = $1 AND account_id = $2 AND statement_date < $3",
		uid, acctID, in.StatementDate).Scan(&lastStmt)
	from := "1970-01-01"
	if lastStmt != nil {
		from = *lastStmt
	}

	var newCharges float64
	if in.NewCharges != nil {
		newCharges = *in.NewCharges
	} else {
		var spend, refund float64
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND account_id = $2 AND type = 'expense' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date > $3 AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date <= $4`,
			uid, acctID, from, in.StatementDate).Scan(&spend)
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
			WHERE user_id = $1 AND account_id = $2 AND type = 'income' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date > $3 AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date <= $4`,
			uid, acctID, from, in.StatementDate).Scan(&refund)
		newCharges = spend - refund
	}

	var prevBalance float64
	var priorTotal *float64
	d.QueryRow(`SELECT amount_due FROM fin_cc_statements
		WHERE user_id = $1 AND account_id = $2 AND statement_date < $3
		ORDER BY statement_date DESC LIMIT 1`, uid, acctID, in.StatementDate).Scan(&priorTotal)
	if priorTotal != nil {
		prevBalance = *priorTotal
	} else {
		var opening float64
		d.QueryRow("SELECT opening_balance FROM fin_accounts WHERE id = $1 AND user_id = $2", acctID, uid).Scan(&opening)
		if opening < 0 {
			prevBalance = -opening
		}
	}
	var payments float64
	d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
		WHERE user_id = $1 AND account_id = $2 AND type = 'transfer_in' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date > $3 AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date <= $4`,
		uid, acctID, from, in.StatementDate).Scan(&payments)
	prevBalance -= payments

	total := prevBalance + newCharges
	if in.AmountDue != nil {
		total = *in.AmountDue
	}

	var cashback float64
	if in.CashbackEarned != nil {
		cashback = *in.CashbackEarned
	} else {
		var cbType string
		var cbVal float64
		d.QueryRow("SELECT cashback_type, cashback_value FROM fin_accounts WHERE id = $1 AND user_id = $2", acctID, uid).Scan(&cbType, &cbVal)
		if cbType == "percentage" && newCharges > 0 {
			cashback = newCharges * cbVal / 100
		} else if cbType == "fixed" {
			cashback = cbVal
		}
	}

	return statementCalcResult{
		AmountDue: total, NewCharges: newCharges, PreviousBalance: prevBalance,
		CashbackEarned: cashback, Payments: payments,
	}
}

func previewStatement(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		acctID, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			StatementDate  string   `json:"statement_date"`
			AmountDue      *float64 `json:"amount_due"`
			NewCharges     *float64 `json:"new_charges"`
			CashbackEarned *float64 `json:"cashback_earned"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.StatementDate == "" {
			b.StatementDate = userNow(d, uid).Format("2006-01-02")
		}

		calc := computeStatementTotals(d, uid, acctID, statementCalcInput{
			StatementDate: b.StatementDate, AmountDue: b.AmountDue,
			NewCharges: b.NewCharges, CashbackEarned: b.CashbackEarned,
		})
		writeJSON(w, 200, map[string]any{
			"statement_date": b.StatementDate,
			"due_date":       deriveDueDate(deps, uid, acctID, b.StatementDate),
			"amount_due":     calc.AmountDue, "new_charges": calc.NewCharges,
			"previous_balance": calc.PreviousBalance, "cashback_earned": calc.CashbackEarned,
			"payments": calc.Payments,
		})
	}
}

// createStatement closes a billing cycle. amount_due (the payable total) =
// previous_balance + new_charges, where:
//   - new_charges     = purchases − refunds in (prev statement, this statement]
//   - previous_balance = prior statement's payable total − payments this cycle
//     (first statement carries any unbilled opening debt instead)
//
// Both components may be negative (overpayment leaves a credit). All three are
// overridable from the client.
func createStatement(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		acctID, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			StatementDate  string   `json:"statement_date"`
			DueDate        string   `json:"due_date"`
			AmountDue      *float64 `json:"amount_due"`  // override payable total
			NewCharges     *float64 `json:"new_charges"` // override cycle charges
			CashbackEarned *float64 `json:"cashback_earned"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.StatementDate == "" {
			b.StatementDate = userNow(d, uid).Format("2006-01-02")
		}

		calc := computeStatementTotals(d, uid, acctID, statementCalcInput{
			StatementDate: b.StatementDate, AmountDue: b.AmountDue,
			NewCharges: b.NewCharges, CashbackEarned: b.CashbackEarned,
		})

		dueDate := b.DueDate
		if dueDate == "" {
			dueDate = deriveDueDate(deps, uid, acctID, b.StatementDate)
		}

		var id int64
		err = d.QueryRow(`INSERT INTO fin_cc_statements
			(user_id, account_id, statement_date, due_date, amount_due, new_charges, previous_balance, cashback_earned)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
			uid, acctID, b.StatementDate, dueDate, calc.AmountDue, calc.NewCharges, calc.PreviousBalance, calc.CashbackEarned,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{
			"id": id, "amount_due": calc.AmountDue, "new_charges": calc.NewCharges,
			"previous_balance": calc.PreviousBalance, "cashback_earned": calc.CashbackEarned,
		})
	}
}

func updateStatement(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var b struct {
			AmountDue       *float64 `json:"amount_due"`
			CashbackEarned  *float64 `json:"cashback_earned"`
			DueDate         *string  `json:"due_date"`
			StatementDate   *string  `json:"statement_date"`
			Paid            *bool    `json:"paid"`
			PaidFromAccount *int64   `json:"paid_from_account"` // bank that pays the bill
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.AmountDue != nil {
			d.Exec("UPDATE fin_cc_statements SET amount_due = $1 WHERE id = $2 AND user_id = $3", *b.AmountDue, id, uid)
		}
		if b.CashbackEarned != nil {
			d.Exec("UPDATE fin_cc_statements SET cashback_earned = $1 WHERE id = $2 AND user_id = $3", *b.CashbackEarned, id, uid)
		}
		if b.DueDate != nil {
			d.Exec("UPDATE fin_cc_statements SET due_date = $1 WHERE id = $2 AND user_id = $3", *b.DueDate, id, uid)
		}
		if b.StatementDate != nil {
			d.Exec("UPDATE fin_cc_statements SET statement_date = $1 WHERE id = $2 AND user_id = $3", *b.StatementDate, id, uid)
		}
		if b.Paid != nil {
			if *b.Paid {
				// Read current state first so the payment posts at most once.
				var alreadyPaid bool
				var cardID int64
				var amountDue float64
				d.QueryRow("SELECT paid, account_id, amount_due FROM fin_cc_statements WHERE id = $1 AND user_id = $2", id, uid).
					Scan(&alreadyPaid, &cardID, &amountDue)
				d.Exec("UPDATE fin_cc_statements SET paid = TRUE, paid_at = NOW() WHERE id = $1 AND user_id = $2", id, uid)
				// Post the bank→card payment: a transfer that reduces what's owed
				// on the card and debits the paying bank account. Only on the
				// first mark-paid, when a source is given and there's a real due.
				if b.PaidFromAccount != nil && !alreadyPaid && amountDue > 0 && *b.PaidFromAccount != cardID {
					today := userNow(d, uid).Format("2006-01-02")
					tx, terr := d.Begin()
					if terr == nil {
						var outID int64
						if e := tx.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, linked_account)
							VALUES ($1,$2,'transfer_out',$3,$4,($5::timestamp AT TIME ZONE 'Asia/Kolkata'),$6) RETURNING id`,
							uid, *b.PaidFromAccount, amountDue, "Credit card payment", today, cardID).Scan(&outID); e == nil {
							var inID int64
							tx.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, linked_account, transfer_pair)
								VALUES ($1,$2,'transfer_in',$3,$4,($5::timestamp AT TIME ZONE 'Asia/Kolkata'),$6,$7) RETURNING id`,
								uid, cardID, amountDue, "Credit card payment", today, *b.PaidFromAccount, outID).Scan(&inID)
							tx.Exec("UPDATE fin_transactions SET transfer_pair = $1 WHERE id = $2", inID, outID)
							tx.Commit()
						} else {
							tx.Rollback()
						}
					}
				}
			} else {
				d.Exec("UPDATE fin_cc_statements SET paid = FALSE, paid_at = NULL WHERE id = $1 AND user_id = $2", id, uid)
			}
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteStatement(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM fin_cc_statements WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// --- overview --------------------------------------------------------------

func financeOverview(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())

		// Asset & liability totals from accounts
		type AcctBreakdown struct {
			AccountID int64   `json:"account_id"`
			Name      string  `json:"name"`
			Type      string  `json:"type"`
			Balance   float64 `json:"balance"`
			Color     string  `json:"color"`
		}
		var accts []AcctBreakdown
		rows, _ := d.Query(`SELECT id, name, type, color FROM fin_accounts WHERE user_id = $1 AND archived = FALSE`, uid)
		if rows != nil {
			for rows.Next() {
				var a AcctBreakdown
				rows.Scan(&a.AccountID, &a.Name, &a.Type, &a.Color)
				a.Balance = computeBalance(deps, uid, a.AccountID)
				accts = append(accts, a)
			}
			rows.Close()
		}

		var totalAssets, totalLiabilities float64
		for _, a := range accts {
			if a.Type == "credit_card" {
				if a.Balance < 0 {
					totalLiabilities += -a.Balance
				} else {
					totalAssets += a.Balance
				}
			} else if a.Balance > 0 {
				totalAssets += a.Balance
			} else {
				totalLiabilities += -a.Balance
			}
		}

		// Investments add to assets (current_value)
		var invTotal float64
		d.QueryRow("SELECT COALESCE(SUM(current_value),0) FROM fin_investments WHERE user_id = $1", uid).Scan(&invTotal)
		totalAssets += invTotal

		// Unpaid CC due adds to liabilities (already counted via account balance)

		netWorth := totalAssets - totalLiabilities

		// Current month income / expenses. Month boundaries must be in the
		// user's tz, not the server's (Cloud Run = UTC), or the 1st/last day
		// straddles wrong for IST users near midnight.
		now := time.Now().In(userLocation(d, uid))
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
		monthEnd := now.Format("2006-01-02")
		var monthIncome, monthExpense float64
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND type = 'income' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3`, uid, monthStart, monthEnd).Scan(&monthIncome)
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND type = 'expense' AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3`, uid, monthStart, monthEnd).Scan(&monthExpense)

		// Top expense categories this month
		type CatStat struct {
			ID     *int64  `json:"id"`
			Name   string  `json:"name"`
			Color  string  `json:"color"`
			Amount float64 `json:"amount"`
		}
		var topCats []CatStat
		crows, _ := d.Query(`SELECT c.id, COALESCE(c.name, 'Uncategorized'), COALESCE(c.color, '#6B7280'), SUM(t.amount)
			FROM fin_transactions t LEFT JOIN fin_categories c ON c.id = t.category_id
			WHERE t.user_id = $1 AND t.type = 'expense' AND (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3
			GROUP BY c.id, c.name, c.color ORDER BY SUM(t.amount) DESC LIMIT 8`, uid, monthStart, monthEnd)
		if crows != nil {
			for crows.Next() {
				var c CatStat
				crows.Scan(&c.ID, &c.Name, &c.Color, &c.Amount)
				topCats = append(topCats, c)
			}
			crows.Close()
		}

		// Daily spend trend — last 30 days
		type DayPoint struct {
			Date    string  `json:"date"`
			Income  float64 `json:"income"`
			Expense float64 `json:"expense"`
		}
		trendStart := now.AddDate(0, 0, -29).Format("2006-01-02")
		drows, _ := d.Query(`SELECT (txn_at AT TIME ZONE 'Asia/Kolkata')::date::text AS d,
			COALESCE(SUM(CASE WHEN type='income' THEN amount END), 0),
			COALESCE(SUM(CASE WHEN type='expense' THEN amount END), 0)
			FROM fin_transactions WHERE user_id = $1 AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date >= $2 AND type IN ('income','expense')
			GROUP BY d ORDER BY d ASC`, uid, trendStart)
		var trend []DayPoint
		if drows != nil {
			for drows.Next() {
				var p DayPoint
				drows.Scan(&p.Date, &p.Income, &p.Expense)
				trend = append(trend, p)
			}
			drows.Close()
		}

		// Upcoming CC dues
		type Upcoming struct {
			ID          int64   `json:"id"`
			AccountName string  `json:"account_name"`
			DueDate     string  `json:"due_date"`
			AmountDue   float64 `json:"amount_due"`
			Paid        bool    `json:"paid"`
		}
		var upcoming []Upcoming
		urows, _ := d.Query(`SELECT s.id, a.name, s.due_date::text, s.amount_due, s.paid
			FROM fin_cc_statements s JOIN fin_accounts a ON a.id = s.account_id
			WHERE s.user_id = $1 AND s.paid = FALSE ORDER BY s.due_date ASC LIMIT 5`, uid)
		if urows != nil {
			for urows.Next() {
				var u Upcoming
				urows.Scan(&u.ID, &u.AccountName, &u.DueDate, &u.AmountDue, &u.Paid)
				upcoming = append(upcoming, u)
			}
			urows.Close()
		}

		// Upcoming billers — next 14d, plus any past-due rows still pending.
		type UpcomingBill struct {
			ID             int64   `json:"id"`
			Name           string  `json:"name"`
			Amount         float64 `json:"amount"`
			DueDate        string  `json:"due_date"`
			AccountName    *string `json:"account_name"`
			IsSubscription bool    `json:"is_subscription"`
			AutoRenew      bool    `json:"auto_renew"`
		}
		var upcomingBills []UpcomingBill
		brows, _ := d.Query(`SELECT b.id, b.name, b.amount, b.next_due_date::text, a.name,
			b.is_subscription, b.auto_renew
			FROM fin_billers b LEFT JOIN fin_accounts a ON a.id = b.account_id
			WHERE b.user_id = $1 AND b.archived = FALSE
			  AND b.next_due_date <= (CURRENT_DATE + INTERVAL '14 days')
			ORDER BY b.next_due_date ASC LIMIT 8`, uid)
		if brows != nil {
			for brows.Next() {
				var u UpcomingBill
				brows.Scan(&u.ID, &u.Name, &u.Amount, &u.DueDate, &u.AccountName, &u.IsSubscription, &u.AutoRenew)
				upcomingBills = append(upcomingBills, u)
			}
			brows.Close()
		}
		if upcomingBills == nil {
			upcomingBills = []UpcomingBill{}
		}

		// Investments distribution
		type InvBreak struct {
			Type   string  `json:"type"`
			Amount float64 `json:"amount"`
		}
		var invBreak []InvBreak
		ibrows, _ := d.Query(`SELECT type, COALESCE(SUM(current_value),0) FROM fin_investments WHERE user_id = $1 GROUP BY type`, uid)
		if ibrows != nil {
			for ibrows.Next() {
				var b InvBreak
				ibrows.Scan(&b.Type, &b.Amount)
				invBreak = append(invBreak, b)
			}
			ibrows.Close()
		}

		// Monthly recurring investments outflow
		var monthlyInvest float64
		d.QueryRow(`SELECT COALESCE(SUM(monthly_amount),0) FROM fin_investments WHERE user_id = $1 AND frequency = 'monthly'`, uid).Scan(&monthlyInvest)

		if accts == nil {
			accts = []AcctBreakdown{}
		}
		if topCats == nil {
			topCats = []CatStat{}
		}
		if trend == nil {
			trend = []DayPoint{}
		}
		if upcoming == nil {
			upcoming = []Upcoming{}
		}
		if invBreak == nil {
			invBreak = []InvBreak{}
		}

		writeJSON(w, 200, map[string]any{
			"net_worth":              netWorth,
			"total_assets":           totalAssets,
			"total_liabilities":      totalLiabilities,
			"investments_total":      invTotal,
			"month_income":           monthIncome,
			"month_expense":          monthExpense,
			"month_savings":          monthIncome - monthExpense,
			"month_recurring_invest": monthlyInvest,
			"accounts":               accts,
			"top_expense_categories": topCats,
			"daily_trend":            trend,
			"upcoming_dues":          upcoming,
			"upcoming_bills":         upcomingBills,
			"investments_breakdown":  invBreak,
		})
	}
}

func networthHistory(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`SELECT snapshot_date::text, assets, liabilities, net_worth
			FROM fin_networth_snapshots WHERE user_id = $1 ORDER BY snapshot_date ASC LIMIT 365`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type Snap struct {
			Date        string  `json:"date"`
			Assets      float64 `json:"assets"`
			Liabilities float64 `json:"liabilities"`
			NetWorth    float64 `json:"net_worth"`
		}
		var out []Snap
		for rows.Next() {
			var s Snap
			rows.Scan(&s.Date, &s.Assets, &s.Liabilities, &s.NetWorth)
			out = append(out, s)
		}
		if out == nil {
			out = []Snap{}
		}
		writeJSON(w, 200, out)
	}
}

// networthSnapshot writes today's snapshot, replacing if it exists.
// Called manually by the user via the UI.
func networthSnapshot(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())

		var assets, liabilities float64
		rows, _ := d.Query(`SELECT id, type FROM fin_accounts WHERE user_id = $1 AND archived = FALSE`, uid)
		if rows != nil {
			for rows.Next() {
				var aid int64
				var atype string
				rows.Scan(&aid, &atype)
				bal := computeBalance(deps, uid, aid)
				if atype == "credit_card" {
					if bal < 0 {
						liabilities += -bal
					} else {
						assets += bal
					}
				} else if bal > 0 {
					assets += bal
				} else {
					liabilities += -bal
				}
			}
			rows.Close()
		}
		var invTotal float64
		d.QueryRow("SELECT COALESCE(SUM(current_value),0) FROM fin_investments WHERE user_id = $1", uid).Scan(&invTotal)
		assets += invTotal

		netWorth := assets - liabilities
		today := userNow(d, uid).Format("2006-01-02")
		_, err := d.Exec(`INSERT INTO fin_networth_snapshots (user_id, snapshot_date, assets, liabilities, net_worth)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (user_id, snapshot_date) DO UPDATE SET assets = EXCLUDED.assets, liabilities = EXCLUDED.liabilities, net_worth = EXCLUDED.net_worth`,
			uid, today, assets, liabilities, netWorth)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{
			"date": today, "assets": assets, "liabilities": liabilities, "net_worth": netWorth,
		})
	}
}

// --- exports ---------------------------------------------------------------

func writeCSVHeader(w http.ResponseWriter, filename string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
}

func exportTransactionsCSV(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		writeCSVHeader(w, "sajni_transactions.csv")
		cw := csv.NewWriter(w)
		defer cw.Flush()
		cw.Write([]string{"date", "account", "type", "category", "amount", "description"})
		rows, _ := d.Query(`SELECT (t.txn_at AT TIME ZONE 'Asia/Kolkata')::date::text, a.name, t.type, COALESCE(c.name,''), t.amount, t.description
			FROM fin_transactions t
			JOIN fin_accounts a ON a.id = t.account_id
			LEFT JOIN fin_categories c ON c.id = t.category_id
			WHERE t.user_id = $1 ORDER BY t.txn_at ASC, t.id ASC`, uid)
		if rows != nil {
			for rows.Next() {
				var date, acct, ttype, cat, desc string
				var amount float64
				rows.Scan(&date, &acct, &ttype, &cat, &amount, &desc)
				cw.Write([]string{date, acct, ttype, cat, strconv.FormatFloat(amount, 'f', 2, 64), desc})
			}
			rows.Close()
		}
	}
}

func exportBudgetsCSV(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		writeCSVHeader(w, "sajni_budgets.csv")
		cw := csv.NewWriter(w)
		defer cw.Flush()
		now := userNow(d, uid)
		cw.Write([]string{"budget", "period", "window_start", "window_end", "pockets_filter", "category", "allocated", "spent"})
		rows, _ := d.Query(`SELECT b.id, b.name, b.period, b.start_date::text, b.end_date::text, b.total_amount
			FROM fin_budgets WHERE user_id = $1 ORDER BY b.start_date ASC`, uid)
		if rows != nil {
			for rows.Next() {
				var bid int64
				var name, period, sd, ed string
				var total float64
				rows.Scan(&bid, &name, &period, &sd, &ed, &total)
				ws, we := budgetWindow(period, sd, ed, "", now)
				// pocket filter (names for the CSV, ids for the spent query)
				var pocketIDs []int64
				var pocketNames []string
				prows, _ := d.Query(`SELECT p.id, p.name FROM fin_budget_pockets bp
					JOIN fin_pockets p ON p.id = bp.pocket_id WHERE bp.budget_id = $1`, bid)
				if prows != nil {
					for prows.Next() {
						var pid int64
						var pname string
						prows.Scan(&pid, &pname)
						pocketIDs = append(pocketIDs, pid)
						pocketNames = append(pocketNames, pname)
					}
					prows.Close()
				}
				irows, _ := d.Query(`SELECT COALESCE(c.name,''), bi.category_id, bi.amount
					FROM fin_budget_items bi LEFT JOIN fin_categories c ON c.id = bi.category_id
					WHERE bi.budget_id = $1`, bid)
				if irows != nil {
					for irows.Next() {
						var cat string
						var catID sql.NullInt64
						var alloc, spent float64
						irows.Scan(&cat, &catID, &alloc)
						if catID.Valid {
							// caps inherit the budget's pocket lens
							spent = categorySpent(d, uid, catID.Int64, ws, we, pocketIDs)
						}
						cw.Write([]string{
							name, period, ws, we, strings.Join(pocketNames, ";"), cat,
							strconv.FormatFloat(alloc, 'f', 2, 64),
							strconv.FormatFloat(spent, 'f', 2, 64),
						})
					}
					irows.Close()
				}
				// overall row: the budget's filtered total spend
				cw.Write([]string{
					name, period, ws, we, strings.Join(pocketNames, ";"), "TOTAL",
					strconv.FormatFloat(total, 'f', 2, 64),
					strconv.FormatFloat(budgetSpent(d, uid, ws, we, pocketIDs), 'f', 2, 64),
				})
			}
			rows.Close()
		}
	}
}

func exportNetworthCSV(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		writeCSVHeader(w, "sajni_networth.csv")
		cw := csv.NewWriter(w)
		defer cw.Flush()
		cw.Write([]string{"date", "assets", "liabilities", "net_worth"})
		rows, _ := d.Query(`SELECT snapshot_date::text, assets, liabilities, net_worth
			FROM fin_networth_snapshots WHERE user_id = $1 ORDER BY snapshot_date ASC`, uid)
		if rows != nil {
			for rows.Next() {
				var date string
				var a, l, n float64
				rows.Scan(&date, &a, &l, &n)
				cw.Write([]string{
					date,
					strconv.FormatFloat(a, 'f', 2, 64),
					strconv.FormatFloat(l, 'f', 2, 64),
					strconv.FormatFloat(n, 'f', 2, 64),
				})
			}
			rows.Close()
		}
	}
}

// categorizeTransaction asks the AI service to map a short expense
// (or income) title to one of the user's existing categories. Returns
// {category_id, category_name}. category_id is null when no match (the
// user gets "Others" client-side and can override).
//
// Sits behind the shared AI limiter — categorize counts toward the
// same hourly budget as chat/palette, so it can't be used to siphon
// quota away from the assistant.
func categorizeTransaction(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AI == nil {
			errJSON(w, http.StatusServiceUnavailable, "AI is not configured on this server")
			return
		}
		uid := userID(r.Context())

		var body struct {
			Title string `json:"title"`
			Kind  string `json:"kind"` // "expense" or "income"
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		title := strings.TrimSpace(body.Title)
		if title == "" {
			errJSON(w, 400, "missing title")
			return
		}
		// Defensive input cap — service will trim again, but this
		// keeps obviously-abusive bodies out of the AI path entirely.
		if len(title) > 200 {
			title = title[:200]
		}
		kind := body.Kind
		if kind != "income" {
			kind = "expense"
		}

		// Rate-limit before touching the model.
		if ok, retryAfter := deps.AILimiter.check(uid); !ok {
			secs := int(math.Ceil(retryAfter.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
			errJSON(w, 429, "AI hourly limit reached — try again later")
			return
		}

		// Pull the user's category names for the requested kind.
		rows, err := d.Query(
			`SELECT id, name FROM fin_categories WHERE user_id = $1 AND kind = $2 ORDER BY name`,
			uid, kind,
		)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		type cat struct {
			id   int64
			name string
		}
		var cats []cat
		names := make([]string, 0, 16)
		for rows.Next() {
			var c cat
			if err := rows.Scan(&c.id, &c.name); err == nil {
				cats = append(cats, c)
				names = append(names, c.name)
			}
		}

		// Hard 5s ceiling — categorize is meant to feel typing-paced.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		picked, tokens, err := deps.AI.CategorizeExpense(ctx, title, kind, names)
		if err != nil {
			// Don't 500 — fall back to Others so the form still works.
			picked = "Others"
		}
		// Record usage regardless of outcome so failed calls still count
		// against quota (prevents retry loops from bypassing the cap).
		if tokens <= 0 {
			tokens = 50 // conservative floor for cap accounting
		}
		deps.AILimiter.record(uid, tokens)

		// Look up the id for the matched category. If the user has no
		// explicit "Others", id stays null and the client shows the
		// label without a binding.
		var matchedID *int64
		matchedName := picked
		// Treat "Other" and "Others" as the same bucket so a legacy "Other"
		// category still binds when the model picks the canonical "Others".
		isOthers := func(s string) bool { return strings.EqualFold(s, "Others") || strings.EqualFold(s, "Other") }
		for _, c := range cats {
			if strings.EqualFold(c.name, picked) || (isOthers(picked) && isOthers(c.name)) {
				id := c.id
				matchedID = &id
				matchedName = c.name
				break
			}
		}

		writeJSON(w, 200, map[string]any{
			"category_id":   matchedID,
			"category_name": matchedName,
		})
	}
}

// parseTransactionMessage reads a shared bank / UPI message (SMS or
// notification text) and returns structured transaction fields for the
// share-target confirm sheet to pre-fill. Best-effort and behind the shared
// AI limiter — a parse failure still returns 200 with empty fields so the
// sheet opens for manual entry.
// learnMerchantCategory remembers (or refreshes) the category a merchant is
// filed under. No-op when merchant or category is empty. merchant is keyed
// lowercase so casing doesn't fork the rule.
func learnMerchantCategory(d *db.DB, uid, merchant string, categoryID *int64) {
	m := strings.ToLower(strings.TrimSpace(merchant))
	if m == "" || categoryID == nil {
		return
	}
	d.Exec(`INSERT INTO fin_merchant_categories (user_id, merchant, category_id)
	        VALUES ($1, $2, $3)
	        ON CONFLICT (user_id, merchant)
	        DO UPDATE SET category_id = EXCLUDED.category_id, updated_at = NOW()`,
		uid, m, *categoryID)
}

// lookupMerchantCategory returns the learned (category_id, name) for a
// merchant, or (nil,"") when no rule exists (or its category was deleted).
func lookupMerchantCategory(d *db.DB, uid, merchant string) (*int64, string) {
	m := strings.ToLower(strings.TrimSpace(merchant))
	if m == "" {
		return nil, ""
	}
	var id int64
	var name string
	err := d.QueryRow(`SELECT mc.category_id, c.name
	                   FROM fin_merchant_categories mc
	                   JOIN fin_categories c ON c.id = mc.category_id AND c.user_id = mc.user_id
	                   WHERE mc.user_id = $1 AND mc.merchant = $2`, uid, m).Scan(&id, &name)
	if err != nil {
		return nil, ""
	}
	return &id, name
}

// matchAccountByHint resolves a parsed account_hint (e.g. "2805", "XX2805",
// "Kotak") to one of the user's accounts via each account's comma-separated
// match_hints. A numeric token must equal a full digit run in the hint, or be a
// >=4-digit suffix of one — exact last-4, NOT the loose substring the web
// client used (which mis-matched a short/other token onto the wrong account, so
// the confirm sheet showed "matched" but never switched to the right account).
// Text tokens (bank names, >=3 chars) match as a case-insensitive substring.
// Numeric matches (more specific) win over name matches. Returns nil,false when
// nothing matches.
func matchAccountByHint(d *db.DB, uid, hint string) *int64 {
	h := strings.ToLower(strings.TrimSpace(hint))
	if h == "" {
		return nil
	}
	runs := hintDigitRuns(h)

	rows, err := d.Query(`SELECT id, COALESCE(match_hints,'') FROM fin_accounts WHERE user_id=$1 AND archived=false`, uid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type cand struct {
		id    int64
		hints string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.hints) == nil {
			cands = append(cands, c)
		}
	}

	// Pass 1: numeric exact / last-4 suffix (most specific).
	for _, c := range cands {
		for _, tok := range splitMatchHints(c.hints) {
			if !isAllDigits(tok) {
				continue
			}
			for _, run := range runs {
				if run == tok || (len(tok) >= 4 && strings.HasSuffix(run, tok)) {
					id := c.id
					return &id
				}
			}
		}
	}
	// Pass 2: bank-name substring.
	for _, c := range cands {
		for _, tok := range splitMatchHints(c.hints) {
			if !isAllDigits(tok) && len(tok) >= 3 && strings.Contains(h, tok) {
				id := c.id
				return &id
			}
		}
	}
	return nil
}

func splitMatchHints(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hintDigitRuns returns the maximal consecutive-digit substrings of s
// (e.g. "a/c xx2805 on card 12" -> ["2805", "12"]).
func hintDigitRuns(s string) []string {
	var runs []string
	start := -1
	for i, r := range s {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			runs = append(runs, s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		runs = append(runs, s[start:])
	}
	return runs
}

func parseTransactionMessage(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AI == nil {
			errJSON(w, http.StatusServiceUnavailable, "AI is not configured on this server")
			return
		}
		uid := userID(r.Context())

		var body struct {
			Text string `json:"text"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		text := strings.TrimSpace(body.Text)
		if text == "" {
			errJSON(w, 400, "missing text")
			return
		}
		if len(text) > 2000 {
			text = text[:2000]
		}

		if ok, retryAfter := deps.AILimiter.check(uid); !ok {
			secs := int(math.Ceil(retryAfter.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
			errJSON(w, 429, "AI hourly limit reached — try again later")
			return
		}

		loc := userLocation(deps.DB, uid)
		now := userNow(deps.DB, uid)
		today := now.Format("2006-01-02")
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()

		parsed, tokens, err := deps.AI.ParseTransactionMessage(ctx, text, today)
		if tokens <= 0 {
			tokens = 80 // conservative floor for cap accounting
		}
		deps.AILimiter.record(uid, tokens)
		if err != nil {
			// Best-effort: empty fields → sheet opens for manual entry.
			writeJSON(w, 200, map[string]any{
				"amount": 0, "type": "expense", "description": "", "note": "",
				"txn_at": now.Format(time.RFC3339), "account_hint": "", "account_id": nil, "category_id": nil, "category_name": "",
			})
			return
		}

		// Resolve a category for the parsed merchant. Prefer a learned rule
		// (instant, no AI); fall back to AI inference against the user's
		// categories. Either way the client gets a pre-filled, editable pick.
		var catID *int64
		var catName string
		if parsed.Amount > 0 && strings.TrimSpace(parsed.Description) != "" {
			if id, name := lookupMerchantCategory(deps.DB, uid, parsed.Description); id != nil {
				catID, catName = id, name
			} else {
				kind := parsed.Type
				if kind != "income" {
					kind = "expense"
				}
				type cat struct {
					id   int64
					name string
				}
				var cats []cat
				var names []string
				if crows, qerr := deps.DB.Query(`SELECT id, name FROM fin_categories WHERE user_id=$1 AND kind=$2 ORDER BY name`, uid, kind); qerr == nil {
					for crows.Next() {
						var c cat
						if crows.Scan(&c.id, &c.name) == nil {
							cats = append(cats, c)
							names = append(names, c.name)
						}
					}
					crows.Close()
				}
				picked, ctoks, cerr := deps.AI.CategorizeExpense(ctx, parsed.Description, kind, names)
				if ctoks <= 0 {
					ctoks = 30
				}
				deps.AILimiter.record(uid, ctoks)
				if cerr == nil {
					isOthers := func(s string) bool { return strings.EqualFold(s, "Others") || strings.EqualFold(s, "Other") }
					for _, c := range cats {
						if strings.EqualFold(c.name, picked) || (isOthers(picked) && isOthers(c.name)) {
							id := c.id
							catID, catName = &id, c.name
							break
						}
					}
				}
			}
		}

		// Resolve the matched account server-side (single source of truth for
		// web + android) so the confirm sheet can pre-select it.
		var acctID *int64
		if parsed.Amount > 0 {
			acctID = matchAccountByHint(deps.DB, uid, parsed.AccountHint)
		}

		// Compose the prefill instant: parsed date + parsed time, falling back
		// to the current minute when the message stated no time.
		txnAt := composeTxnAt(loc, parsed.Date, parsed.Time, now)
		writeJSON(w, 200, map[string]any{
			"amount":        parsed.Amount,
			"type":          parsed.Type,
			"description":   parsed.Description,
			"note":          parsed.Note,
			"txn_at":        txnAt.Format(time.RFC3339),
			"account_hint":  parsed.AccountHint,
			"account_id":    acctID,
			"category_id":   catID,
			"category_name": catName,
		})
	}
}
