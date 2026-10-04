package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sajni/internal/db"
)

// Paid-for / settle flow: lends are whole transactions picked from the
// ledger rather than typed in. See db/lends.go for the allocation model.

func lendError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, db.ErrLendInput) {
		errJSON(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), db.ErrLendInput.Error()+": "))
		return
	}
	internalError(w, r, op, err)
}

type lendSettlementResp struct {
	ID            int64   `json:"id"`
	TransactionID int64   `json:"transaction_id"`
	AccountID     int64   `json:"account_id"`
	Account       string  `json:"account"`
	Amount        float64 `json:"amount"`
	Applied       float64 `json:"applied"`
	Description   string  `json:"description"`
	TxnAt         string  `json:"txn_at"`
}

type lendPersonResp struct {
	Borrower    string               `json:"borrower"`
	Outstanding float64              `json:"outstanding"`
	Credit      float64              `json:"credit"` // settled beyond what is owed
	Open        int                  `json:"open"`
	LastAt      string               `json:"last_at"`
	Settlements []lendSettlementResp `json:"settlements"`
}

// loadLendPeople groups lends per person (case-insensitive name) with their
// running balance and settlement credits. Bounded by the lend count.
func loadLendPeople(r *http.Request, d *db.DB, uid string) ([]lendPersonResp, error) {
	ctx := r.Context()
	loc := userLocation(d, uid)
	rows, err := d.QueryContext(ctx, `
		WITH owed AS (
			SELECT LOWER(BTRIM(l.borrower)) AS k,
			       (ARRAY_AGG(l.borrower ORDER BY l.lent_at DESC))[1] AS name,
			       SUM(GREATEST(l.principal - COALESCE(p.repaid,0), 0)) AS outstanding,
			       COUNT(*) FILTER (WHERE l.principal - COALESCE(p.repaid,0) > 0.004) AS open,
			       MAX(l.lent_at) AS last_at
			FROM fin_lends l
			LEFT JOIN (SELECT lend_id, SUM(amount) AS repaid FROM fin_lend_repayments WHERE user_id = $1 GROUP BY lend_id) p ON p.lend_id = l.id
			WHERE l.user_id = $1 GROUP BY 1
		), credit AS (
			SELECT LOWER(BTRIM(s.borrower)) AS k,
			       (ARRAY_AGG(s.borrower ORDER BY t.txn_at DESC))[1] AS name,
			       SUM(t.amount) - COALESCE(SUM(a.applied),0) AS credit,
			       MAX(t.txn_at) AS last_at
			FROM fin_lend_settlements s
			JOIN fin_transactions t ON t.id = s.transaction_id
			LEFT JOIN (SELECT transaction_id, SUM(amount) AS applied FROM fin_lend_repayments WHERE user_id = $1 GROUP BY transaction_id) a ON a.transaction_id = s.transaction_id
			WHERE s.user_id = $1 GROUP BY 1
		)
		SELECT COALESCE(o.k, c.k), COALESCE(o.name, c.name), COALESCE(o.outstanding,0), COALESCE(c.credit,0),
		       COALESCE(o.open,0), GREATEST(o.last_at, c.last_at)
		FROM owed o FULL JOIN credit c ON c.k = o.k
		WHERE COALESCE(o.k, '') <> '' OR c.credit > 0.004
		ORDER BY (COALESCE(o.outstanding,0) > 0.004) DESC, 6 DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []lendPersonResp{}
	byKey := map[string]int{}
	for rows.Next() {
		var p lendPersonResp
		var k string
		var last time.Time
		if err := rows.Scan(&k, &p.Borrower, &p.Outstanding, &p.Credit, &p.Open, &last); err != nil {
			return nil, err
		}
		p.Outstanding = roundMoney(p.Outstanding)
		p.Credit = roundMoney(p.Credit)
		p.LastAt = last.In(loc).Format(time.RFC3339)
		p.Settlements = []lendSettlementResp{}
		byKey[k] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	srows, err := d.QueryContext(ctx, `
		SELECT LOWER(BTRIM(s.borrower)), s.id, t.id, t.account_id, a.name, t.amount, t.description, t.txn_at,
		       COALESCE((SELECT SUM(r.amount) FROM fin_lend_repayments r WHERE r.transaction_id = t.id),0)
		FROM fin_lend_settlements s
		JOIN fin_transactions t ON t.id = s.transaction_id
		JOIN fin_accounts a ON a.id = t.account_id
		WHERE s.user_id = $1 ORDER BY t.txn_at DESC, t.id DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var k string
		var s lendSettlementResp
		var at time.Time
		if err := srows.Scan(&k, &s.ID, &s.TransactionID, &s.AccountID, &s.Account, &s.Amount, &s.Description, &at, &s.Applied); err != nil {
			return nil, err
		}
		s.Amount = roundMoney(s.Amount)
		s.Applied = roundMoney(s.Applied)
		s.TxnAt = at.In(loc).Format(time.RFC3339)
		if i, ok := byKey[k]; ok {
			out[i].Settlements = append(out[i].Settlements, s)
		}
	}
	return out, srows.Err()
}

func listLendPeople(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		people, err := loadLendPeople(r, deps.DB, userID(r.Context()))
		if err != nil {
			internalError(w, r, "list lend people", err)
			return
		}
		writeJSON(w, http.StatusOK, people)
	}
}

type lendCandidateResp struct {
	ID            int64   `json:"id"`
	AccountID     int64   `json:"account_id"`
	Account       string  `json:"account"`
	AccountType   string  `json:"account_type"`
	CategoryName  *string `json:"category_name"`
	CategoryColor *string `json:"category_color"`
	Amount        float64 `json:"amount"`
	Description   string  `json:"description"`
	TxnAt         string  `json:"txn_at"`
}

// listLendCandidates pages through transactions that can be marked:
// kind=paid_for → plain expenses, kind=settle → plain credits. Keyset
// pagination on (txn_at, id) rides idx_fin_transactions_at, so it stays cheap
// however long the ledger grows. cursor = "<unix-nanos>_<id>" from `next`.
func listLendCandidates(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		typ := "expense"
		if queryParam(r, "kind") == "settle" {
			typ = "income"
		}
		clauses := []string{"t.user_id = $1", "t.type = $2", "t.transfer_pair IS NULL"}
		args := []any{uid, typ}
		if typ == "expense" {
			clauses = append(clauses, "NOT EXISTS (SELECT 1 FROM fin_investment_contributions c WHERE c.txn_id = t.id)")
		}
		if v := queryParam(r, "account_id"); v != "" {
			args = append(args, v)
			clauses = append(clauses, "t.account_id = $"+itoa(len(args)))
		}
		if q := strings.TrimSpace(queryParam(r, "q")); q != "" {
			args = append(args, "%"+q+"%")
			n := itoa(len(args))
			clauses = append(clauses, "(t.description ILIKE $"+n+" OR t.note ILIKE $"+n+")")
		}
		if c := queryParam(r, "cursor"); c != "" {
			at, id, ok := strings.Cut(c, "_")
			nanos, err1 := strconv.ParseInt(at, 10, 64)
			txnID, err2 := strconv.ParseInt(id, 10, 64)
			if !ok || err1 != nil || err2 != nil {
				errJSON(w, http.StatusBadRequest, "invalid cursor")
				return
			}
			args = append(args, time.Unix(0, nanos).UTC(), txnID)
			clauses = append(clauses, "(t.txn_at, t.id) < ($"+itoa(len(args)-1)+", $"+itoa(len(args))+")")
		}
		limit := 30
		if n, err := strconv.Atoi(queryParam(r, "limit")); err == nil && n > 0 && n <= 100 {
			limit = n
		}
		rows, err := d.QueryContext(r.Context(), `SELECT t.id, t.account_id, a.name, a.type, c.name, c.color, t.amount, t.description, t.txn_at
			FROM fin_transactions t
			JOIN fin_accounts a ON a.id = t.account_id
			LEFT JOIN fin_categories c ON c.id = t.category_id
			WHERE `+strings.Join(clauses, " AND ")+`
			ORDER BY t.txn_at DESC, t.id DESC LIMIT `+itoa(limit+1), args...)
		if err != nil {
			internalError(w, r, "list lend candidates", err)
			return
		}
		defer rows.Close()
		loc := userLocation(d, uid)
		items := []lendCandidateResp{}
		var ats []time.Time
		for rows.Next() {
			var c lendCandidateResp
			var at time.Time
			if err := rows.Scan(&c.ID, &c.AccountID, &c.Account, &c.AccountType, &c.CategoryName, &c.CategoryColor, &c.Amount, &c.Description, &at); err != nil {
				internalError(w, r, "scan lend candidate", err)
				return
			}
			c.Amount = roundMoney(c.Amount)
			c.TxnAt = at.In(loc).Format(time.RFC3339)
			items = append(items, c)
			ats = append(ats, at)
		}
		if err := rows.Err(); err != nil {
			internalError(w, r, "read lend candidates", err)
			return
		}
		var next *string
		if len(items) > limit {
			items = items[:limit]
			cursor := strconv.FormatInt(ats[limit-1].UnixNano(), 10) + "_" + strconv.FormatInt(items[limit-1].ID, 10)
			next = &cursor
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "next": next})
	}
}

func markPaidFor(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Borrower       string  `json:"borrower"`
			TransactionIDs []int64 `json:"transaction_ids"`
			DueDate        *string `json:"due_date"` // absent → card cycle due date; "" → none
			Remind         bool    `json:"remind"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		if body.DueDate != nil && *body.DueDate != "" {
			if _, err := time.Parse("2006-01-02", *body.DueDate); err != nil {
				errJSON(w, http.StatusBadRequest, "invalid due_date")
				return
			}
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin paid for", err)
			return
		}
		defer tx.Rollback()
		ids, err := db.MarkPaidFor(ctx, tx, uid, body.Borrower, body.TransactionIDs, body.DueDate, body.Remind, userLocation(deps.DB, uid))
		if err != nil {
			lendError(w, r, "mark paid for", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit paid for", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"lend_ids": ids})
	}
}

// settleLends marks existing credits (transaction_ids) as settlements from a
// person, or records money received with no transaction yet (account_id +
// amount) and settles it.
func settleLends(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Borrower       string  `json:"borrower"`
			TransactionIDs []int64 `json:"transaction_ids"`
			AccountID      int64   `json:"account_id"`
			Amount         float64 `json:"amount"`
			ReceivedAt     string  `json:"received_at"`
			Note           string  `json:"note"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin settle", err)
			return
		}
		defer tx.Rollback()
		var newTxnID int64
		if len(body.TransactionIDs) > 0 {
			err = db.SettleWith(ctx, tx, uid, body.Borrower, body.TransactionIDs)
		} else {
			at := resolveTxnAt(body.ReceivedAt, userNow(deps.DB, uid))
			newTxnID, err = db.RecordSettlement(ctx, tx, uid, body.Borrower, body.AccountID, body.Amount, at, body.Note)
		}
		if err != nil {
			lendError(w, r, "settle", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit settle", err)
			return
		}
		if newTxnID != 0 {
			syncTags(deps.DB, uid, "transaction", newTxnID, body.Note)
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
	}
}

func deleteSettlement(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin unsettle", err)
			return
		}
		defer tx.Rollback()
		if err := db.Unsettle(ctx, tx, uid, id); err != nil {
			lendError(w, r, "unsettle", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit unsettle", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
