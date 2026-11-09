package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"sajni/internal/db"
)

// Money the user owes people (db/owed.go): borrow and pay back by marking
// transactions, add a debt from before Sajni, net off against what they owe.

func registerOwedRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /api/finance/owed", createOwed(deps))
	mux.HandleFunc("POST /api/finance/owed/borrow", markBorrow(deps))
	mux.HandleFunc("POST /api/finance/owed/payback", markPayBack(deps))
	mux.HandleFunc("POST /api/finance/owed/netoff", netOff(deps))
	mux.HandleFunc("PUT /api/finance/owed/{id}", updateOwed(deps))
	mux.HandleFunc("DELETE /api/finance/owed/{id}", deleteOwed(deps))
}

type owedEntryResp struct {
	ID            int64   `json:"id"`
	TransactionID int64   `json:"transaction_id"`
	Kind          string  `json:"kind"` // debt | payment
	Type          string  `json:"type"` // borrow | owed | borrow_repayment | netoff
	AccountID     int64   `json:"account_id"`
	Account       string  `json:"account"`
	Amount        float64 `json:"amount"`
	Description   string  `json:"description"`
	Note          string  `json:"note"`
	TxnAt         string  `json:"txn_at"`
	DueDate       *string `json:"due_date"`
}

type owedPerson struct {
	Person string  `json:"person"`
	Amount float64 `json:"amount"`
}

// loadOwedPeople is what the user owes, in total and per person (only those
// still owed something).
func loadOwedPeople(ctx context.Context, d *db.DB, uid string) (float64, []owedPerson, error) {
	rows, err := d.QueryContext(ctx, `SELECT (ARRAY_AGG(o.person ORDER BY t.txn_at DESC))[1],
		SUM(CASE WHEN o.kind = 'debt' THEN t.amount ELSE -t.amount END)
		FROM fin_owed o JOIN fin_transactions t ON t.id = o.transaction_id
		WHERE o.user_id = $1 GROUP BY LOWER(BTRIM(o.person))
		HAVING SUM(CASE WHEN o.kind = 'debt' THEN t.amount ELSE -t.amount END) > 0.004
		ORDER BY 2 DESC`, uid)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	out := []owedPerson{}
	var total float64
	for rows.Next() {
		var p owedPerson
		if err := rows.Scan(&p.Person, &p.Amount); err != nil {
			return 0, nil, err
		}
		p.Amount = roundMoney(p.Amount)
		total += p.Amount
		out = append(out, p)
	}
	return roundMoney(total), out, rows.Err()
}

// addOwedToPeople merges what the user owes into the People list: each
// person's you_owe and entries, and people the user only owes. Active people
// (either side open) lead, then the most recent.
func addOwedToPeople(ctx context.Context, d *db.DB, uid string, loc *time.Location, people []lendPersonResp) ([]lendPersonResp, error) {
	rows, err := d.QueryContext(ctx, `SELECT LOWER(BTRIM(o.person)), o.person, o.id, t.id, o.kind, t.type, t.account_id, a.name,
		t.amount, t.description, t.note, t.txn_at, o.due_date::text
		FROM fin_owed o JOIN fin_transactions t ON t.id = o.transaction_id JOIN fin_accounts a ON a.id = t.account_id
		WHERE o.user_id = $1 ORDER BY t.txn_at, t.id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string]int{}
	for i, p := range people {
		byKey[strings.ToLower(strings.TrimSpace(p.Borrower))] = i
	}
	for rows.Next() {
		var k, name string
		var e owedEntryResp
		var at time.Time
		if err := rows.Scan(&k, &name, &e.ID, &e.TransactionID, &e.Kind, &e.Type, &e.AccountID, &e.Account,
			&e.Amount, &e.Description, &e.Note, &at, &e.DueDate); err != nil {
			return nil, err
		}
		e.Amount = roundMoney(e.Amount)
		e.TxnAt = at.In(loc).Format(time.RFC3339)
		i, ok := byKey[k]
		if !ok {
			people = append(people, lendPersonResp{Borrower: name, Settlements: []lendSettlementResp{}, LastAt: e.TxnAt})
			i = len(people) - 1
			byKey[k] = i
		}
		p := &people[i]
		if e.Kind == "debt" {
			p.YouOwe += e.Amount
		} else {
			p.YouOwe -= e.Amount
		}
		p.Owed = append(p.Owed, e)
		if e.TxnAt > p.LastAt {
			p.LastAt = e.TxnAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range people {
		people[i].YouOwe = roundMoney(max(people[i].YouOwe, 0))
		if people[i].Owed == nil {
			people[i].Owed = []owedEntryResp{}
		}
	}
	active := func(p lendPersonResp) bool { return p.Outstanding > 0.004 || p.YouOwe > 0.004 }
	sort.SliceStable(people, func(a, b int) bool {
		if active(people[a]) != active(people[b]) {
			return active(people[a])
		}
		return people[a].LastAt > people[b].LastAt
	})
	return people, nil
}

func ownedAccount(ctx context.Context, d *db.DB, uid string, id int64) bool {
	var ok bool
	d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_accounts WHERE id = $1 AND user_id = $2)`, id, uid).Scan(&ok)
	return ok
}

// createOwed records a new debt: money borrowed into an account, or (no
// money moved) owed from before Sajni.
func createOwed(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Person       string  `json:"person"`
			AccountID    int64   `json:"account_id"`
			Amount       float64 `json:"amount"`
			Description  string  `json:"description"`
			Note         string  `json:"note"`
			At           string  `json:"at"`
			DueDate      *string `json:"due_date"`
			NoMoneyMoved bool    `json:"no_money_moved"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		ctx := r.Context()
		if !ownedAccount(ctx, deps.DB, uid, body.AccountID) {
			errJSON(w, http.StatusNotFound, "That account no longer exists.")
			return
		}
		slate, err := plainSlateID(deps.DB, uid)
		if err != nil {
			internalError(w, r, "resolve slate", err)
			return
		}
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin owed", err)
			return
		}
		defer tx.Rollback()
		id, err := db.AddDebt(ctx, tx, uid, db.OwedDebt{
			Person: body.Person, AccountID: body.AccountID, Amount: body.Amount, Description: body.Description,
			Note: body.Note, At: resolveTxnAt(body.At, userNow(deps.DB, uid)), SlateID: slate,
			Due: body.DueDate, NoMoneyMoved: body.NoMoneyMoved,
		})
		if err != nil {
			lendError(w, r, "add debt", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit owed", err)
			return
		}
		syncTags(deps.DB, uid, "transaction", id, body.Note)
		writeJSON(w, http.StatusCreated, map[string]int64{"transaction_id": id})
	}
}

// markBorrow turns existing income into money the user owes back.
func markBorrow(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			TransactionID int64   `json:"transaction_id"`
			Person        string  `json:"person"`
			DueDate       *string `json:"due_date"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin borrow", err)
			return
		}
		defer tx.Rollback()
		if err := db.Borrow(ctx, tx, uid, body.Person, body.TransactionID, body.DueDate); err != nil {
			lendError(w, r, "mark borrow", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit borrow", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// markPayBack turns an existing expense into paying someone back.
func markPayBack(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			TransactionID int64  `json:"transaction_id"`
			Person        string `json:"person"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin pay back", err)
			return
		}
		defer tx.Rollback()
		if err := db.PayBack(ctx, tx, uid, body.Person, body.TransactionID); err != nil {
			lendError(w, r, "mark pay back", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit pay back", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func netOff(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Person string `json:"person"`
			At     string `json:"at"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin net off", err)
			return
		}
		defer tx.Rollback()
		amount, err := db.NetOff(ctx, tx, uid, body.Person, resolveTxnAt(body.At, userNow(deps.DB, uid)))
		if err != nil {
			lendError(w, r, "net off", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit net off", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]float64{"amount": amount})
	}
}

// updateOwed changes when the user means to pay a debt back.
func updateOwed(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		var body struct {
			DueDate string `json:"due_date"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin owed update", err)
			return
		}
		defer tx.Rollback()
		if err := db.SetOwedDue(ctx, tx, uid, id, body.DueDate); err != nil {
			lendError(w, r, "set owed due", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit owed update", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// deleteOwed undoes a borrow or payment (the transaction goes back to income
// or an expense), deletes a debt that moved no money, or undoes a net-off.
func deleteOwed(deps Deps) http.HandlerFunc {
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
			internalError(w, r, "begin owed delete", err)
			return
		}
		defer tx.Rollback()
		if err := db.Unowe(ctx, tx, uid, id); err != nil {
			lendError(w, r, "undo owed", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit owed delete", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
