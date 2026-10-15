package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"sajni/internal/db"
)

func registerLendRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/finance/lends", listLends(deps))
	mux.HandleFunc("POST /api/finance/lends", createLend(deps))
	mux.HandleFunc("PUT /api/finance/lends/{id}", updateLend(deps))
	mux.HandleFunc("DELETE /api/finance/lends/{id}", deleteLend(deps))
	mux.HandleFunc("GET /api/finance/lends/people", listLendPeople(deps))
	mux.HandleFunc("GET /api/finance/lends/candidates", listLendCandidates(deps))
	mux.HandleFunc("POST /api/finance/lends/paid-for", markPaidFor(deps))
	mux.HandleFunc("POST /api/finance/lends/settle", settleLends(deps))
	mux.HandleFunc("PUT /api/finance/lends/settlements/{id}", updateSettlement(deps))
	mux.HandleFunc("DELETE /api/finance/lends/settlements/{id}", deleteSettlement(deps))
	mux.HandleFunc("POST /api/finance/lends/forgive", forgiveLends(deps))
}

type lendRepaymentResp struct {
	ID                   int64   `json:"id"`
	DestinationAccountID int64   `json:"destination_account_id"`
	DestinationAccount   string  `json:"destination_account"`
	Amount               float64 `json:"amount"`
	RepaidAt             string  `json:"repaid_at"`
	Note                 string  `json:"note"`
	TransactionID        int64   `json:"transaction_id"`
	Settled              bool    `json:"settled"`  // derived from a settlement credit
	Forgiven             bool    `json:"forgiven"` // derived from a writeoff
}

type lendResp struct {
	ID                int64   `json:"id"`
	SourceAccountID   int64   `json:"source_account_id"`
	SourceAccount     string  `json:"source_account"`
	SourceAccountType string  `json:"source_account_type"`
	Borrower          string  `json:"borrower"`
	Principal         float64 `json:"principal"`
	Repaid            float64 `json:"repaid"`
	Outstanding       float64 `json:"outstanding"`
	Description       string  `json:"description"`
	Note              string  `json:"note"`
	LentAt            string  `json:"lent_at"`
	DueDate           *string `json:"due_date"`
	Remind            bool    `json:"remind"`
	Status            string  `json:"status"`
	Origin            string  `json:"origin"`
	// Split: a share cut from a bill. Its title, account, date and note are
	// the bill's (edited on the bill); the lend holds who, share and due.
	Split      bool                `json:"split"`
	Repayments []lendRepaymentResp `json:"repayments"` // pinned payments it covers
}

type lendAsset struct {
	ID          int64   `json:"id"`
	Borrower    string  `json:"borrower"`
	Description string  `json:"description"`
	Outstanding float64 `json:"outstanding"`
}

func listLends(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lends, err := loadLends(r.Context(), deps.DB, userID(r.Context()), userLocation(deps.DB, userID(r.Context())))
		if err != nil {
			internalError(w, r, "list lends", err)
			return
		}
		writeJSON(w, http.StatusOK, lends)
	}
}

func loadLends(ctx context.Context, d *db.DB, uid string, loc *time.Location) ([]lendResp, error) {
	// The title is the transaction's (a split share mirrors its bill's).
	rows, err := d.QueryContext(ctx, `
		SELECT l.id, l.source_account_id, a.name, a.type, l.borrower, l.principal,
		       st.repaid, st.outstanding, COALESCE(NULLIF(t.description,''), l.description), l.note, l.lent_at,
		       l.due_date::text, l.remind, l.origin, t.split_of IS NOT NULL
		FROM fin_lends l
		JOIN fin_lend_state st ON st.lend_id = l.id
		JOIN fin_accounts a ON a.id = l.source_account_id AND a.user_id = l.user_id
		JOIN fin_transactions t ON t.id = l.source_transaction_id
		WHERE l.user_id = $1
		ORDER BY (st.outstanding > 0) DESC, l.lent_at DESC, l.id DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []lendResp{}
	byID := map[int64]int{}
	for rows.Next() {
		var item lendResp
		var lentAt time.Time
		if err := rows.Scan(&item.ID, &item.SourceAccountID, &item.SourceAccount, &item.SourceAccountType,
			&item.Borrower, &item.Principal, &item.Repaid, &item.Outstanding, &item.Description, &item.Note,
			&lentAt, &item.DueDate, &item.Remind, &item.Origin, &item.Split); err != nil {
			return nil, err
		}
		item.Principal = roundMoney(item.Principal)
		item.Repaid = roundMoney(item.Repaid)
		item.Outstanding = roundMoney(item.Outstanding)
		item.Status = "open"
		if item.Outstanding == 0 {
			item.Status = "settled"
		}
		item.LentAt = lentAt.In(loc).Format(time.RFC3339)
		item.Repayments = []lendRepaymentResp{}
		byID[item.ID] = len(out)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	repaymentRows, err := d.QueryContext(ctx, `
		SELECT r.id, r.lend_id, r.destination_account_id, a.name, r.amount, r.repaid_at, r.note, r.transaction_id,
		       EXISTS (SELECT 1 FROM fin_lend_settlements s WHERE s.transaction_id = r.transaction_id), t.type = 'writeoff'
		FROM fin_lend_repayments r
		JOIN fin_accounts a ON a.id = r.destination_account_id AND a.user_id = r.user_id
		JOIN fin_transactions t ON t.id = r.transaction_id
		WHERE r.user_id = $1 ORDER BY r.repaid_at DESC, r.id DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer repaymentRows.Close()
	for repaymentRows.Next() {
		var repayment lendRepaymentResp
		var lendID int64
		var repaidAt time.Time
		if err := repaymentRows.Scan(&repayment.ID, &lendID, &repayment.DestinationAccountID,
			&repayment.DestinationAccount, &repayment.Amount, &repaidAt, &repayment.Note,
			&repayment.TransactionID, &repayment.Settled, &repayment.Forgiven); err != nil {
			return nil, err
		}
		repayment.Amount = roundMoney(repayment.Amount)
		repayment.RepaidAt = repaidAt.In(loc).Format(time.RFC3339)
		if index, ok := byID[lendID]; ok {
			out[index].Repayments = append(out[index].Repayments, repayment)
		}
	}
	return out, repaymentRows.Err()
}

func createLend(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			SourceAccountID int64   `json:"source_account_id"`
			Borrower        string  `json:"borrower"`
			Amount          float64 `json:"amount"`
			Description     string  `json:"description"`
			Note            string  `json:"note"`
			LentAt          string  `json:"lent_at"`
			DueDate         *string `json:"due_date"`
			Remind          bool    `json:"remind"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		borrower := strings.TrimSpace(body.Borrower)
		if borrower == "" || body.SourceAccountID == 0 || body.Amount <= 0 {
			errJSON(w, http.StatusBadRequest, "Enter who it's for, the account it came from and an amount above zero.")
			return
		}
		description := strings.TrimSpace(body.Description)
		if description == "" {
			errJSON(w, http.StatusBadRequest, errTitleRequired)
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
			internalError(w, r, "begin lend", err)
			return
		}
		defer tx.Rollback()
		if err := requireOwnedFinanceRef(ctx, tx, "fin_accounts", uid, body.SourceAccountID); err != nil {
			errJSON(w, http.StatusNotFound, "That account no longer exists.")
			return
		}
		plainID, err := plainSlateID(deps.DB, uid)
		if err != nil {
			internalError(w, r, "resolve lend slate", err)
			return
		}
		lentAt := resolveTxnAt(body.LentAt, userNow(deps.DB, uid))
		var txnID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions
			(user_id, account_id, type, amount, description, note, txn_at, slate_id)
			VALUES ($1,$2,'lend',$3,$4,$5,$6,$7) RETURNING id`,
			uid, body.SourceAccountID, roundMoney(body.Amount), description, strings.TrimSpace(body.Note), lentAt, plainID,
		).Scan(&txnID); err != nil {
			internalError(w, r, "create lend transaction", err)
			return
		}
		var lendID int64
		var due any
		if body.DueDate != nil && *body.DueDate != "" {
			due = *body.DueDate
		}
		remind := body.Remind && due != nil
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_lends
			(user_id, source_account_id, source_transaction_id, borrower, principal, description, note, lent_at, due_date, remind)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
			uid, body.SourceAccountID, txnID, borrower, roundMoney(body.Amount), description,
			strings.TrimSpace(body.Note), lentAt, due, remind,
		).Scan(&lendID); err != nil {
			internalError(w, r, "create lend", err)
			return
		}
		if err := db.RebalanceLends(ctx, tx, uid, borrower); err != nil {
			lendError(w, r, "rebalance lends", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit lend", err)
			return
		}
		syncTags(deps.DB, uid, "transaction", txnID, body.Note)
		writeJSON(w, http.StatusCreated, map[string]int64{"id": lendID, "transaction_id": txnID})
	}
}

// updateLend edits a lend. A whole-transaction lend owns its title, account,
// date and note; a split share's are the bill's, so only who, their share and
// the due date change here. Changing who it's for drops its pinned payments
// back to the old person's overall total.
func updateLend(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		var body struct {
			SourceAccountID *int64   `json:"source_account_id"`
			Amount          *float64 `json:"amount"`
			Borrower        string   `json:"borrower"`
			Description     string   `json:"description"`
			Note            string   `json:"note"`
			LentAt          *string  `json:"lent_at"`
			DueDate         string   `json:"due_date"`
			Remind          bool     `json:"remind"`
		}
		if err := readJSON(r, &body); err != nil || strings.TrimSpace(body.Borrower) == "" {
			errJSON(w, http.StatusBadRequest, "Enter who it's for.")
			return
		}
		borrower := strings.TrimSpace(body.Borrower)
		if body.DueDate != "" {
			if _, err := time.Parse("2006-01-02", body.DueDate); err != nil {
				errJSON(w, http.StatusBadRequest, "Pick a valid due date.")
				return
			}
		}
		if body.Amount != nil && *body.Amount <= 0 {
			errJSON(w, http.StatusBadRequest, "Enter an amount above zero.")
			return
		}
		var requestedLentAt *time.Time
		if body.LentAt != nil {
			parsed, err := time.Parse(time.RFC3339, *body.LentAt)
			if err != nil {
				errJSON(w, http.StatusBadRequest, "Pick a valid date.")
				return
			}
			requestedLentAt = &parsed
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin lend update", err)
			return
		}
		defer tx.Rollback()
		var txnID, sourceAccountID int64
		var principal float64
		var lentAt time.Time
		var oldBorrower, origin string
		var split bool
		if err := tx.QueryRowContext(ctx, `SELECT l.source_transaction_id, l.source_account_id, l.principal, l.lent_at, l.borrower, l.origin,
			t.split_of IS NOT NULL
			FROM fin_lends l JOIN fin_transactions t ON t.id = l.source_transaction_id
			WHERE l.id=$1 AND l.user_id=$2 FOR UPDATE OF l`, id, uid,
		).Scan(&txnID, &sourceAccountID, &principal, &lentAt, &oldBorrower, &origin, &split); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				errJSON(w, http.StatusNotFound, "That lend no longer exists.")
			} else {
				internalError(w, r, "update lend", err)
			}
			return
		}
		description := strings.TrimSpace(body.Description)
		if !split && description == "" {
			errJSON(w, http.StatusBadRequest, errTitleRequired)
			return
		}
		// A paid-for lend is part of a bill: its amount is the borrower's
		// share, and changing it moves the line between their part and the
		// user's (the bill's total, and so the card, never changes).
		if origin == "paid_for" && body.Amount != nil && roundMoney(*body.Amount) != roundMoney(principal) {
			if err := db.SetPaidForShare(ctx, tx, uid, id, *body.Amount); err != nil {
				lendError(w, r, "set paid-for share", err)
				return
			}
			if err := tx.QueryRowContext(ctx, `SELECT l.source_transaction_id, l.principal, t.split_of IS NOT NULL
				FROM fin_lends l JOIN fin_transactions t ON t.id = l.source_transaction_id WHERE l.id=$1`, id).Scan(&txnID, &principal, &split); err != nil {
				internalError(w, r, "reload lend", err)
				return
			}
		} else if body.Amount != nil {
			principal = roundMoney(*body.Amount)
		}
		if !strings.EqualFold(oldBorrower, borrower) {
			if _, err := tx.ExecContext(ctx, `UPDATE fin_lend_settlements SET lend_id = NULL WHERE lend_id = $1 AND user_id = $2`, id, uid); err != nil {
				internalError(w, r, "unpin payments", err)
				return
			}
		}
		if split {
			// The bill owns title, account, date and note.
			if _, err := tx.ExecContext(ctx, `UPDATE fin_lends SET borrower=$1, due_date=NULLIF($2,'')::date, remind=($3 AND $2<>''),
				last_reminded_due_date=CASE WHEN due_date IS DISTINCT FROM NULLIF($2,'')::date THEN NULL ELSE last_reminded_due_date END,
				updated_at=NOW() WHERE id=$4 AND user_id=$5`, borrower, body.DueDate, body.Remind, id, uid); err != nil {
				internalError(w, r, "update lend", err)
				return
			}
		} else {
			if body.SourceAccountID != nil {
				if err := requireOwnedFinanceRef(ctx, tx, "fin_accounts", uid, *body.SourceAccountID); err != nil {
					errJSON(w, http.StatusNotFound, "That account no longer exists.")
					return
				}
				sourceAccountID = *body.SourceAccountID
			}
			if requestedLentAt != nil {
				lentAt = *requestedLentAt
			}
			if _, err := tx.ExecContext(ctx, `UPDATE fin_lends SET source_account_id=$1, borrower=$2,
				principal=$3, description=$4, note=$5, lent_at=$6,
				due_date=NULLIF($7,'')::date, remind=($8 AND $7<>''),
				last_reminded_due_date=CASE WHEN due_date IS DISTINCT FROM NULLIF($7,'')::date THEN NULL ELSE last_reminded_due_date END,
				updated_at=NOW() WHERE id=$9 AND user_id=$10`, sourceAccountID,
				borrower, principal, description, strings.TrimSpace(body.Note),
				lentAt, body.DueDate, body.Remind, id, uid); err != nil {
				internalError(w, r, "update lend", err)
				return
			}
			if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET account_id=$1, amount=$2,
				description=$3, note=$4, txn_at=$5, updated_at=NOW()
				WHERE id=$6 AND user_id=$7`, sourceAccountID, principal, description,
				strings.TrimSpace(body.Note), lentAt, txnID, uid); err != nil {
				internalError(w, r, "update lend transaction", err)
				return
			}
		}
		for _, b := range []string{oldBorrower, borrower} {
			if err := db.RebalanceLends(ctx, tx, uid, b); err != nil {
				lendError(w, r, "rebalance lends", err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit lend update", err)
			return
		}
		if !split {
			syncTags(deps.DB, uid, "transaction", txnID, body.Note)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func deleteLend(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		lendID, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin lend delete", err)
			return
		}
		defer tx.Rollback()
		var sourceTxnID int64
		var borrower, origin string
		if err := tx.QueryRowContext(ctx, `SELECT source_transaction_id, borrower, origin FROM fin_lends WHERE id=$1 AND user_id=$2 FOR UPDATE`, lendID, uid).Scan(&sourceTxnID, &borrower, &origin); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				errJSON(w, http.StatusNotFound, "That lend no longer exists.")
			} else {
				internalError(w, r, "find lend", err)
			}
			return
		}
		if origin == "paid_for" {
			// The transaction pre-existed: hand it back as an expense.
			if err := db.UnmarkPaidFor(ctx, tx, uid, lendID); err != nil {
				lendError(w, r, "unmark paid for", err)
				return
			}
			if err := tx.Commit(); err != nil {
				internalError(w, r, "commit unmark", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		// Payments pinned to it fall back to the person's overall total.
		txnIDs := []int64{sourceTxnID}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_lends WHERE id=$1 AND user_id=$2`, lendID, uid); err != nil {
			internalError(w, r, "delete lend", err)
			return
		}
		for _, txnID := range txnIDs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE user_id=$1 AND id=$2`, uid, txnID); err != nil {
				internalError(w, r, "delete lend transaction", err)
				return
			}
		}
		if err := db.RebalanceLends(ctx, tx, uid, borrower); err != nil {
			lendError(w, r, "rebalance lends", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit lend delete", err)
			return
		}
		for _, txnID := range txnIDs {
			deps.DB.ExecContext(ctx, `DELETE FROM tags WHERE user_id=$1 AND entity_type='transaction' AND entity_id=$2`, uid, txnID)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func loadOutstandingLends(ctx context.Context, d *db.DB, uid string) (float64, []lendAsset, error) {
	rows, err := d.QueryContext(ctx, `SELECT l.id, l.borrower, COALESCE(NULLIF(t.description,''), l.description), st.outstanding
		FROM fin_lends l JOIN fin_lend_state st ON st.lend_id = l.id
		JOIN fin_transactions t ON t.id = l.source_transaction_id
		WHERE l.user_id=$1 AND st.outstanding > 0
		ORDER BY l.lent_at, l.id`, uid)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	assets := []lendAsset{}
	for rows.Next() {
		var asset lendAsset
		if err := rows.Scan(&asset.ID, &asset.Borrower, &asset.Description, &asset.Outstanding); err != nil {
			return 0, nil, err
		}
		asset.Outstanding = roundMoney(asset.Outstanding)
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	// Owed overall: every lend minus every payment (money held for someone
	// beyond what they owe nets off, as before).
	var total float64
	if err := d.QueryRowContext(ctx, `SELECT COALESCE((SELECT SUM(principal) FROM fin_lends WHERE user_id = $1),0)
		- COALESCE((SELECT SUM(t.amount) FROM fin_lend_settlements s JOIN fin_transactions t ON t.id = s.transaction_id WHERE s.user_id = $1),0)`, uid).Scan(&total); err != nil {
		return 0, nil, err
	}
	return roundMoney(total), assets, nil
}
