package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
}

// --- helpers ---------------------------------------------------------------

var defaultExpenseCategories = []struct {
	Name  string
	Color string
	Icon  string
}{
	{"Food & Dining", "#F97316", "utensils"},
	{"Groceries", "#84CC16", "shopping-basket"},
	{"Transport", "#3B82F6", "car"},
	{"Rent", "#8B5CF6", "home"},
	{"Utilities", "#06B6D4", "plug"},
	{"Shopping", "#EC4899", "shopping-bag"},
	{"Entertainment", "#A855F7", "film"},
	{"Health", "#EF4444", "heart-pulse"},
	{"Travel", "#0EA5E9", "plane"},
	{"Education", "#F59E0B", "book-open"},
	{"Subscriptions", "#6366F1", "repeat"},
	{"Other", "#6B7280", "circle"},
}

var defaultIncomeCategories = []struct {
	Name  string
	Color string
	Icon  string
}{
	{"Salary", "#10B981", "wallet"},
	{"Interest", "#22C55E", "trending-up"},
	{"Dividends", "#14B8A6", "pie-chart"},
	{"Refund", "#0D9488", "rotate-ccw"},
	{"Gift", "#F472B6", "gift"},
	{"Other", "#6B7280", "circle"},
}

func seedDefaultCategoriesIfEmpty(deps Deps, uid string) {
	d := deps.DB
	var cnt int
	d.QueryRow("SELECT COUNT(*) FROM fin_categories WHERE user_id = $1", uid).Scan(&cnt)
	if cnt > 0 {
		return
	}
	for _, c := range defaultExpenseCategories {
		d.Exec("INSERT INTO fin_categories (user_id, name, kind, color, icon) VALUES ($1, $2, 'expense', $3, $4)", uid, c.Name, c.Color, c.Icon)
	}
	for _, c := range defaultIncomeCategories {
		d.Exec("INSERT INTO fin_categories (user_id, name, kind, color, icon) VALUES ($1, $2, 'income', $3, $4)", uid, c.Name, c.Color, c.Icon)
	}
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
	Color          string   `json:"color"`
	Archived       bool     `json:"archived"`
	CreatedAt      string   `json:"created_at"`
}

func listAccounts(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`SELECT id, name, type, institution, currency, opening_balance,
			credit_limit, statement_day, due_day, cashback_type, cashback_value, color, archived, created_at
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
				&a.CreditLimit, &a.StatementDay, &a.DueDay, &a.CashbackType, &a.CashbackValue, &a.Color, &a.Archived, &a.CreatedAt)
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
			(user_id, name, type, institution, currency, opening_balance, credit_limit, statement_day, due_day, cashback_type, cashback_value, color)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
			uid, b.Name, b.Type, b.Institution, b.Currency, b.OpeningBalance,
			b.CreditLimit, b.StatementDay, b.DueDay, b.CashbackType, b.CashbackValue, b.Color,
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
		seedDefaultCategoriesIfEmpty(deps, uid)
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
		if b.Color == "" {
			b.Color = "#6B7280"
		}
		var id int64
		err := d.QueryRow(
			"INSERT INTO fin_categories (user_id, name, kind, color, icon) VALUES ($1,$2,$3,$4,$5) RETURNING id",
			uid, b.Name, b.Kind, b.Color, b.Icon,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
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
			d.Exec("UPDATE fin_categories SET name = $1 WHERE id = $2 AND user_id = $3", *b.Name, id, uid)
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
	TxnDate       string  `json:"txn_date"`
	TransferPair  *int64  `json:"transfer_pair"`
	LinkedAccount *int64  `json:"linked_account"`
	CreatedAt     string  `json:"created_at"`
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
			clauses = append(clauses, "t.txn_date >= $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if v := queryParam(r, "to"); v != "" {
			clauses = append(clauses, "t.txn_date <= $"+itoa(ph))
			args = append(args, v)
			ph++
		}
		if v := queryParam(r, "search"); v != "" {
			clauses = append(clauses, "t.description ILIKE $"+itoa(ph))
			args = append(args, "%"+v+"%")
			ph++
		}

		limit := 200
		if v := queryParam(r, "limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}

		q := `SELECT t.id, t.account_id, a.name, t.category_id, c.name, c.color, t.type, t.amount,
			  t.description, t.txn_date::text, t.transfer_pair, t.linked_account, t.created_at::text
			  FROM fin_transactions t
			  JOIN fin_accounts a ON a.id = t.account_id
			  LEFT JOIN fin_categories c ON c.id = t.category_id
			  WHERE ` + strings.Join(clauses, " AND ") +
			` ORDER BY t.txn_date DESC, t.id DESC LIMIT ` + itoa(limit)

		rows, err := d.Query(q, args...)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		var out []txnResp
		for rows.Next() {
			var t txnResp
			rows.Scan(&t.ID, &t.AccountID, &t.AccountName, &t.CategoryID, &t.CategoryName, &t.CategoryColor,
				&t.Type, &t.Amount, &t.Description, &t.TxnDate, &t.TransferPair, &t.LinkedAccount, &t.CreatedAt)
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
			TxnDate       string  `json:"txn_date"`
			LinkedAccount *int64  `json:"linked_account"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.AccountID == 0 {
			errJSON(w, 400, "account_id required")
			return
		}
		if b.Type == "" {
			b.Type = "expense"
		}
		if b.TxnDate == "" {
			b.TxnDate = time.Now().Format("2006-01-02")
		}

		// Transfer: insert a pair (transfer_out from source, transfer_in to dest)
		if b.Type == "transfer" {
			if b.LinkedAccount == nil || *b.LinkedAccount == b.AccountID {
				errJSON(w, 400, "transfer requires distinct linked_account")
				return
			}
			tx, err := d.Begin()
			if err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			var outID, inID int64
			if err := tx.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_date, linked_account) VALUES ($1,$2,'transfer_out',$3,$4,$5,$6) RETURNING id`,
				uid, b.AccountID, b.Amount, b.Description, b.TxnDate, *b.LinkedAccount).Scan(&outID); err != nil {
				tx.Rollback()
				errJSON(w, 500, err.Error())
				return
			}
			if err := tx.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_date, linked_account, transfer_pair) VALUES ($1,$2,'transfer_in',$3,$4,$5,$6,$7) RETURNING id`,
				uid, *b.LinkedAccount, b.Amount, b.Description, b.TxnDate, b.AccountID, outID).Scan(&inID); err != nil {
				tx.Rollback()
				errJSON(w, 500, err.Error())
				return
			}
			if _, err := tx.Exec("UPDATE fin_transactions SET transfer_pair = $1 WHERE id = $2", inID, outID); err != nil {
				tx.Rollback()
				errJSON(w, 500, err.Error())
				return
			}
			if err := tx.Commit(); err != nil {
				errJSON(w, 500, err.Error())
				return
			}
			writeJSON(w, 201, map[string]int64{"id": outID, "pair_id": inID})
			return
		}

		var id int64
		err := d.QueryRow(`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_date) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			uid, b.AccountID, b.CategoryID, b.Type, b.Amount, b.Description, b.TxnDate,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
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
			CategoryID  *int64   `json:"category_id"`
			Amount      *float64 `json:"amount"`
			Description *string  `json:"description"`
			TxnDate     *string  `json:"txn_date"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.CategoryID != nil {
			d.Exec("UPDATE fin_transactions SET category_id = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *b.CategoryID, id, uid)
		}
		if b.Amount != nil {
			d.Exec("UPDATE fin_transactions SET amount = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *b.Amount, id, uid)
			// keep transfer pair amount in sync
			d.Exec("UPDATE fin_transactions SET amount = $1, updated_at = NOW() WHERE transfer_pair = $2 AND user_id = $3", *b.Amount, id, uid)
		}
		if b.Description != nil {
			d.Exec("UPDATE fin_transactions SET description = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *b.Description, id, uid)
			d.Exec("UPDATE fin_transactions SET description = $1, updated_at = NOW() WHERE transfer_pair = $2 AND user_id = $3", *b.Description, id, uid)
		}
		if b.TxnDate != nil {
			d.Exec("UPDATE fin_transactions SET txn_date = $1, updated_at = NOW() WHERE id = $2 AND user_id = $3", *b.TxnDate, id, uid)
			d.Exec("UPDATE fin_transactions SET txn_date = $1, updated_at = NOW() WHERE transfer_pair = $2 AND user_id = $3", *b.TxnDate, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
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
		// delete pair if any
		var pair *int64
		d.QueryRow("SELECT transfer_pair FROM fin_transactions WHERE id = $1 AND user_id = $2", id, uid).Scan(&pair)
		d.Exec("DELETE FROM fin_transactions WHERE id = $1 AND user_id = $2", id, uid)
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
	TotalAmount float64          `json:"total_amount"`
	Spent       float64          `json:"spent"`
	Items       []budgetItemResp `json:"items"`
}

func listBudgets(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
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
			// items
			itemRows, _ := d.Query(`SELECT id, category_id, amount FROM fin_budget_items WHERE budget_id = $1`, b.ID)
			if itemRows != nil {
				for itemRows.Next() {
					var it budgetItemResp
					itemRows.Scan(&it.ID, &it.CategoryID, &it.Amount)
					if it.CategoryID != nil {
						d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
							WHERE user_id = $1 AND type = 'expense' AND category_id = $2 AND txn_date BETWEEN $3 AND $4`,
							uid, *it.CategoryID, b.StartDate, b.EndDate).Scan(&it.Spent)
					}
					b.Items = append(b.Items, it)
				}
				itemRows.Close()
			}
			if b.Items == nil {
				b.Items = []budgetItemResp{}
			}
			d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
				WHERE user_id = $1 AND type = 'expense' AND txn_date BETWEEN $2 AND $3`,
				uid, b.StartDate, b.EndDate).Scan(&b.Spent)
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
		var id int64
		err := d.QueryRow(`INSERT INTO fin_budgets (user_id, name, period, start_date, end_date, total_amount) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			uid, b.Name, b.Period, b.StartDate, b.EndDate, b.TotalAmount,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		for _, it := range b.Items {
			d.Exec(`INSERT INTO fin_budget_items (budget_id, category_id, amount) VALUES ($1,$2,$3)`, id, it.CategoryID, it.Amount)
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
			StartDate   *string  `json:"start_date"`
			EndDate     *string  `json:"end_date"`
			TotalAmount *float64 `json:"total_amount"`
			Items       *[]struct {
				CategoryID *int64  `json:"category_id"`
				Amount     float64 `json:"amount"`
			} `json:"items"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Name != nil {
			d.Exec("UPDATE fin_budgets SET name = $1 WHERE id = $2 AND user_id = $3", *b.Name, id, uid)
		}
		if b.StartDate != nil {
			d.Exec("UPDATE fin_budgets SET start_date = $1 WHERE id = $2 AND user_id = $3", *b.StartDate, id, uid)
		}
		if b.EndDate != nil {
			d.Exec("UPDATE fin_budgets SET end_date = $1 WHERE id = $2 AND user_id = $3", *b.EndDate, id, uid)
		}
		if b.TotalAmount != nil {
			d.Exec("UPDATE fin_budgets SET total_amount = $1 WHERE id = $2 AND user_id = $3", *b.TotalAmount, id, uid)
		}
		if b.Items != nil {
			d.Exec("DELETE FROM fin_budget_items WHERE budget_id = $1", id)
			for _, it := range *b.Items {
				d.Exec("INSERT INTO fin_budget_items (budget_id, category_id, amount) VALUES ($1,$2,$3)", id, it.CategoryID, it.Amount)
			}
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
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
			frequency, start_date::text, maturity_date::text, expected_return, notes, last_updated::text
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
		}
		var out []Inv
		for rows.Next() {
			var i Inv
			rows.Scan(&i.ID, &i.Name, &i.Type, &i.AccountID, &i.InvestedAmount, &i.CurrentValue, &i.MonthlyAmount,
				&i.Frequency, &i.StartDate, &i.MaturityDate, &i.ExpectedReturn, &i.Notes, &i.LastUpdated)
			out = append(out, i)
		}
		if out == nil {
			out = []Inv{}
		}
		writeJSON(w, 200, out)
	}
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
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if b.Type == "" {
			b.Type = "sip"
		}
		if b.Frequency == "" {
			b.Frequency = "monthly"
		}
		var id int64
		err := d.QueryRow(`INSERT INTO fin_investments
			(user_id, name, type, account_id, invested_amount, current_value, monthly_amount, frequency, start_date, maturity_date, expected_return, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
			uid, b.Name, b.Type, b.AccountID, b.InvestedAmount, b.CurrentValue, b.MonthlyAmount, b.Frequency,
			b.StartDate, b.MaturityDate, b.ExpectedReturn, b.Notes,
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
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
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
			s.amount_due, s.cashback_earned, s.paid, s.paid_at::text
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
			ID             int64   `json:"id"`
			AccountID      int64   `json:"account_id"`
			AccountName    string  `json:"account_name"`
			StatementDate  string  `json:"statement_date"`
			DueDate        string  `json:"due_date"`
			AmountDue      float64 `json:"amount_due"`
			CashbackEarned float64 `json:"cashback_earned"`
			Paid           bool    `json:"paid"`
			PaidAt         *string `json:"paid_at"`
		}
		var out []Stmt
		for rows.Next() {
			var s Stmt
			rows.Scan(&s.ID, &s.AccountID, &s.AccountName, &s.StatementDate, &s.DueDate,
				&s.AmountDue, &s.CashbackEarned, &s.Paid, &s.PaidAt)
			out = append(out, s)
		}
		if out == nil {
			out = []Stmt{}
		}
		writeJSON(w, 200, out)
	}
}

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
			AmountDue      *float64 `json:"amount_due"`
			CashbackEarned *float64 `json:"cashback_earned"`
		}
		if err := readJSON(r, &b); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		// auto-compute amount_due if not provided: sum of expense txns on this card up to statement_date that aren't tied to a previous statement window
		var amount float64
		if b.AmountDue != nil {
			amount = *b.AmountDue
		} else {
			var lastStmt *string
			d.QueryRow("SELECT MAX(statement_date)::text FROM fin_cc_statements WHERE user_id = $1 AND account_id = $2 AND statement_date < $3",
				uid, acctID, b.StatementDate).Scan(&lastStmt)
			from := "1970-01-01"
			if lastStmt != nil {
				from = *lastStmt
			}
			d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions
				WHERE user_id = $1 AND account_id = $2 AND type = 'expense' AND txn_date > $3 AND txn_date <= $4`,
				uid, acctID, from, b.StatementDate).Scan(&amount)
		}
		// auto-compute cashback if not provided
		var cashback float64
		if b.CashbackEarned != nil {
			cashback = *b.CashbackEarned
		} else {
			var cbType string
			var cbVal float64
			d.QueryRow("SELECT cashback_type, cashback_value FROM fin_accounts WHERE id = $1 AND user_id = $2", acctID, uid).Scan(&cbType, &cbVal)
			if cbType == "percentage" {
				cashback = amount * cbVal / 100
			} else if cbType == "fixed" {
				cashback = cbVal
			}
		}
		var id int64
		err = d.QueryRow(`INSERT INTO fin_cc_statements (user_id, account_id, statement_date, due_date, amount_due, cashback_earned)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			uid, acctID, b.StatementDate, b.DueDate, amount, cashback,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"id": id, "amount_due": amount, "cashback_earned": cashback})
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
			AmountDue      *float64 `json:"amount_due"`
			CashbackEarned *float64 `json:"cashback_earned"`
			DueDate        *string  `json:"due_date"`
			StatementDate  *string  `json:"statement_date"`
			Paid           *bool    `json:"paid"`
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
				d.Exec("UPDATE fin_cc_statements SET paid = TRUE, paid_at = NOW() WHERE id = $1 AND user_id = $2", id, uid)
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

		// Current month income / expenses
		now := time.Now()
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local).Format("2006-01-02")
		monthEnd := now.Format("2006-01-02")
		var monthIncome, monthExpense float64
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND type = 'income' AND txn_date BETWEEN $2 AND $3`, uid, monthStart, monthEnd).Scan(&monthIncome)
		d.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM fin_transactions WHERE user_id = $1 AND type = 'expense' AND txn_date BETWEEN $2 AND $3`, uid, monthStart, monthEnd).Scan(&monthExpense)

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
			WHERE t.user_id = $1 AND t.type = 'expense' AND t.txn_date BETWEEN $2 AND $3
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
		drows, _ := d.Query(`SELECT txn_date::text,
			COALESCE(SUM(CASE WHEN type='income' THEN amount END), 0),
			COALESCE(SUM(CASE WHEN type='expense' THEN amount END), 0)
			FROM fin_transactions WHERE user_id = $1 AND txn_date >= $2 AND type IN ('income','expense')
			GROUP BY txn_date ORDER BY txn_date ASC`, uid, trendStart)
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
		today := time.Now().Format("2006-01-02")
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
		rows, _ := d.Query(`SELECT t.txn_date::text, a.name, t.type, COALESCE(c.name,''), t.amount, t.description
			FROM fin_transactions t
			JOIN fin_accounts a ON a.id = t.account_id
			LEFT JOIN fin_categories c ON c.id = t.category_id
			WHERE t.user_id = $1 ORDER BY t.txn_date ASC, t.id ASC`, uid)
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
		cw.Write([]string{"budget", "period", "start_date", "end_date", "category", "allocated", "spent"})
		rows, _ := d.Query(`SELECT b.id, b.name, b.period, b.start_date::text, b.end_date::text
			FROM fin_budgets WHERE user_id = $1 ORDER BY b.start_date ASC`, uid)
		if rows != nil {
			for rows.Next() {
				var bid int64
				var name, period, sd, ed string
				rows.Scan(&bid, &name, &period, &sd, &ed)
				irows, _ := d.Query(`SELECT COALESCE(c.name,''), bi.amount,
					COALESCE((SELECT SUM(t.amount) FROM fin_transactions t WHERE t.user_id = $1 AND t.type = 'expense' AND t.category_id = bi.category_id AND t.txn_date BETWEEN $2 AND $3), 0)
					FROM fin_budget_items bi LEFT JOIN fin_categories c ON c.id = bi.category_id
					WHERE bi.budget_id = $4`, uid, sd, ed, bid)
				if irows != nil {
					for irows.Next() {
						var cat string
						var alloc, spent float64
						irows.Scan(&cat, &alloc, &spent)
						cw.Write([]string{
							name, period, sd, ed, cat,
							strconv.FormatFloat(alloc, 'f', 2, 64),
							strconv.FormatFloat(spent, 'f', 2, 64),
						})
					}
					irows.Close()
				}
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
		var matchedName = picked
		for _, c := range cats {
			if strings.EqualFold(c.name, picked) {
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
