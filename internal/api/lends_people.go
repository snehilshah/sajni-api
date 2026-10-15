package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sajni/internal/db"
)

// Paid-for / settle flow: lends and payments are transactions marked from the
// transaction sheet. See db/lends.go for the pinned / overall model.

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
	Applied       float64 `json:"applied"`  // how much of its pinned lend it covers
	Forgiven      bool    `json:"forgiven"` // a writeoff, not money received
	LendID        *int64  `json:"lend_id"`  // pinned lend; nil = toward the overall total
	Toward        *string `json:"toward"`   // the pinned lend's title
	Description   string  `json:"description"`
	Note          string  `json:"note"`
	TxnAt         string  `json:"txn_at"`
}

type lendPersonResp struct {
	Borrower    string               `json:"borrower"`
	Outstanding float64              `json:"outstanding"`
	Credit      float64              `json:"credit"` // paid beyond what is owed
	Open        int                  `json:"open"`
	LastAt      string               `json:"last_at"`
	Settlements []lendSettlementResp `json:"settlements"`
}

// loadLendPeople groups lends per person (case-insensitive name): what they
// owe overall (lends minus every payment), any surplus, and their payments.
func loadLendPeople(r *http.Request, d *db.DB, uid string) ([]lendPersonResp, error) {
	ctx := r.Context()
	loc := userLocation(d, uid)
	rows, err := d.QueryContext(ctx, `
		WITH owed AS (
			SELECT LOWER(BTRIM(l.borrower)) AS k,
			       (ARRAY_AGG(l.borrower ORDER BY l.lent_at DESC))[1] AS name,
			       SUM(l.principal) AS owed,
			       COUNT(*) FILTER (WHERE st.outstanding > 0.004) AS open,
			       MAX(l.lent_at) AS last_at
			FROM fin_lends l JOIN fin_lend_state st ON st.lend_id = l.id
			WHERE l.user_id = $1 GROUP BY 1
		), paid AS (
			SELECT LOWER(BTRIM(s.borrower)) AS k,
			       (ARRAY_AGG(s.borrower ORDER BY t.txn_at DESC))[1] AS name,
			       SUM(t.amount) AS paid,
			       MAX(t.txn_at) AS last_at
			FROM fin_lend_settlements s
			JOIN fin_transactions t ON t.id = s.transaction_id
			WHERE s.user_id = $1 GROUP BY 1
		), c AS (
			SELECT COALESCE(o.k, p.k) AS k, COALESCE(o.name, p.name) AS name,
			       COALESCE(o.owed,0) - COALESCE(p.paid,0) AS net,
			       COALESCE(o.open,0) AS open, GREATEST(o.last_at, p.last_at) AS last_at
			FROM owed o FULL JOIN paid p ON p.k = o.k
		)
		SELECT c.k, c.name, GREATEST(c.net,0), GREATEST(-c.net,0), c.open, c.last_at
		FROM c
		WHERE c.k <> ''
		ORDER BY (c.net > 0.004) DESC, 6 DESC`, uid)
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
		SELECT LOWER(BTRIM(s.borrower)), s.id, t.id, t.account_id, a.name, t.amount, t.description, t.note, t.txn_at,
		       COALESCE((SELECT SUM(r.amount) FROM fin_lend_repayments r WHERE r.transaction_id = t.id),0), t.type = 'writeoff',
		       s.lend_id, COALESCE(NULLIF(lt.description,''), pl.description)
		FROM fin_lend_settlements s
		JOIN fin_transactions t ON t.id = s.transaction_id
		JOIN fin_accounts a ON a.id = t.account_id
		LEFT JOIN fin_lends pl ON pl.id = s.lend_id
		LEFT JOIN fin_transactions lt ON lt.id = pl.source_transaction_id
		WHERE s.user_id = $1 ORDER BY t.txn_at DESC, t.id DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var k string
		var s lendSettlementResp
		var at time.Time
		if err := srows.Scan(&k, &s.ID, &s.TransactionID, &s.AccountID, &s.Account, &s.Amount, &s.Description, &s.Note, &at, &s.Applied, &s.Forgiven, &s.LendID, &s.Toward); err != nil {
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
			clauses = append(clauses, "NOT EXISTS (SELECT 1 FROM fin_investment_contributions c WHERE c.txn_id = t.id)",
				"NOT EXISTS (SELECT 1 FROM fin_transactions sp WHERE sp.split_of = t.id)")
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
			Share          float64 `json:"share"` // > 0, one transaction: their part of the bill
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		if body.DueDate != nil && *body.DueDate != "" {
			if _, err := time.Parse("2006-01-02", *body.DueDate); err != nil {
				errJSON(w, http.StatusBadRequest, "Pick a valid due date.")
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
		ids, err := db.MarkPaidFor(ctx, tx, uid, body.Borrower, body.TransactionIDs, body.DueDate, body.Remind, userLocation(deps.DB, uid), body.Share)
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

// settleLends marks existing credits as a person paying the user back,
// toward one of their lends (lend_id) or their overall total (omitted).
func settleLends(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Borrower       string  `json:"borrower"`
			TransactionIDs []int64 `json:"transaction_ids"`
			LendID         *int64  `json:"lend_id"`
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
		if err := db.SettleWith(ctx, tx, uid, body.Borrower, body.TransactionIDs, body.LendID); err != nil {
			lendError(w, r, "settle", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit settle", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
	}
}

// updateSettlement re-points a payment: toward a lend (lend_id) or, with
// lend_id null, the person's overall total.
func updateSettlement(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		var body struct {
			LendID *int64 `json:"lend_id"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin settlement update", err)
			return
		}
		defer tx.Rollback()
		if err := db.SetSettlementLend(ctx, tx, uid, id, body.LendID); err != nil {
			lendError(w, r, "update settlement", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit settlement update", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

// forgiveLends writes off part or all of what a person owes (db.Forgive).
func forgiveLends(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Borrower   string  `json:"borrower"`
			Amount     float64 `json:"amount"`
			CategoryID *int64  `json:"category_id"`
			ForgivenAt string  `json:"forgiven_at"`
			Note       string  `json:"note"`
			LendID     *int64  `json:"lend_id"` // toward this lend; omitted = overall
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		ctx := r.Context()
		if body.CategoryID != nil {
			if err := requireOwnedFinanceRef(ctx, deps.DB, "fin_categories", uid, *body.CategoryID); err != nil {
				errJSON(w, http.StatusNotFound, "That category no longer exists.")
				return
			}
		}
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin forgive", err)
			return
		}
		defer tx.Rollback()
		at := resolveTxnAt(body.ForgivenAt, userNow(deps.DB, uid))
		txnID, err := db.Forgive(ctx, tx, uid, body.Borrower, body.Amount, body.CategoryID, at, body.Note, body.LendID)
		if err != nil {
			lendError(w, r, "forgive", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit forgive", err)
			return
		}
		syncTags(deps.DB, uid, "transaction", txnID, body.Note)
		writeJSON(w, http.StatusCreated, map[string]int64{"transaction_id": txnID})
	}
}
