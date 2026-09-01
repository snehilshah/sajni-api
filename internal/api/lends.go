package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
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
	mux.HandleFunc("POST /api/finance/lends/{id}/repayments", createLendRepayment(deps))
	mux.HandleFunc("DELETE /api/finance/lends/{id}/repayments/{repaymentID}", deleteLendRepayment(deps))
}

type lendRepaymentResp struct {
	ID                   int64   `json:"id"`
	DestinationAccountID int64   `json:"destination_account_id"`
	DestinationAccount   string  `json:"destination_account"`
	Amount               float64 `json:"amount"`
	RepaidAt             string  `json:"repaid_at"`
	Note                 string  `json:"note"`
}

type lendResp struct {
	ID                int64               `json:"id"`
	SourceAccountID   int64               `json:"source_account_id"`
	SourceAccount     string              `json:"source_account"`
	SourceAccountType string              `json:"source_account_type"`
	Borrower          string              `json:"borrower"`
	Principal         float64             `json:"principal"`
	Repaid            float64             `json:"repaid"`
	Outstanding       float64             `json:"outstanding"`
	Description       string              `json:"description"`
	Note              string              `json:"note"`
	LentAt            string              `json:"lent_at"`
	DueDate           *string             `json:"due_date"`
	Remind            bool                `json:"remind"`
	Status            string              `json:"status"`
	Repayments        []lendRepaymentResp `json:"repayments"`
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
	rows, err := d.QueryContext(ctx, `
		SELECT l.id, l.source_account_id, a.name, a.type, l.borrower, l.principal,
		       COALESCE(SUM(r.amount),0), l.description, l.note, l.lent_at,
		       l.due_date::text, l.remind
		FROM fin_lends l
		JOIN fin_accounts a ON a.id = l.source_account_id AND a.user_id = l.user_id
		LEFT JOIN fin_lend_repayments r ON r.lend_id = l.id AND r.user_id = l.user_id
		WHERE l.user_id = $1
		GROUP BY l.id, a.name, a.type
		ORDER BY (l.principal - COALESCE(SUM(r.amount),0) > 0) DESC, l.lent_at DESC, l.id DESC`, uid)
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
			&item.Borrower, &item.Principal, &item.Repaid, &item.Description, &item.Note,
			&lentAt, &item.DueDate, &item.Remind); err != nil {
			return nil, err
		}
		item.Principal = roundMoney(item.Principal)
		item.Repaid = roundMoney(item.Repaid)
		item.Outstanding = roundMoney(math.Max(item.Principal-item.Repaid, 0))
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
		SELECT r.id, r.lend_id, r.destination_account_id, a.name, r.amount, r.repaid_at, r.note
		FROM fin_lend_repayments r
		JOIN fin_accounts a ON a.id = r.destination_account_id AND a.user_id = r.user_id
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
			&repayment.DestinationAccount, &repayment.Amount, &repaidAt, &repayment.Note); err != nil {
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
			errJSON(w, http.StatusBadRequest, "borrower, source_account_id and a positive amount are required")
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
			internalError(w, r, "begin lend", err)
			return
		}
		defer tx.Rollback()
		if err := requireOwnedFinanceRef(ctx, tx, "fin_accounts", uid, body.SourceAccountID); err != nil {
			errJSON(w, http.StatusNotFound, "account not found")
			return
		}
		plainID, err := plainSlateID(deps.DB, uid)
		if err != nil {
			internalError(w, r, "resolve lend slate", err)
			return
		}
		lentAt := resolveTxnAt(body.LentAt, userNow(deps.DB, uid))
		description := strings.TrimSpace(body.Description)
		if description == "" {
			description = "Lent to " + borrower
		}
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
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit lend", err)
			return
		}
		syncTags(deps.DB, uid, "transaction", txnID, body.Note)
		writeJSON(w, http.StatusCreated, map[string]int64{"id": lendID, "transaction_id": txnID})
	}
}

func updateLend(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		var body struct {
			Borrower    string `json:"borrower"`
			Description string `json:"description"`
			Note        string `json:"note"`
			DueDate     string `json:"due_date"`
			Remind      bool   `json:"remind"`
		}
		if err := readJSON(r, &body); err != nil || strings.TrimSpace(body.Borrower) == "" {
			errJSON(w, http.StatusBadRequest, "borrower required")
			return
		}
		if body.DueDate != "" {
			if _, err := time.Parse("2006-01-02", body.DueDate); err != nil {
				errJSON(w, http.StatusBadRequest, "invalid due_date")
				return
			}
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin lend update", err)
			return
		}
		defer tx.Rollback()
		var txnID int64
		if err := tx.QueryRowContext(ctx, `UPDATE fin_lends SET borrower=$1, description=$2, note=$3,
			due_date=NULLIF($4,'')::date, remind=($5 AND $4<>''),
			last_reminded_due_date=CASE WHEN due_date IS DISTINCT FROM NULLIF($4,'')::date THEN NULL ELSE last_reminded_due_date END,
			updated_at=NOW() WHERE id=$6 AND user_id=$7 RETURNING source_transaction_id`,
			strings.TrimSpace(body.Borrower), strings.TrimSpace(body.Description), strings.TrimSpace(body.Note),
			body.DueDate, body.Remind, id, uid).Scan(&txnID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				errJSON(w, http.StatusNotFound, "lend not found")
			} else {
				internalError(w, r, "update lend", err)
			}
			return
		}
		description := strings.TrimSpace(body.Description)
		if description == "" {
			description = "Lent to " + strings.TrimSpace(body.Borrower)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET description=$1, note=$2, updated_at=NOW()
			WHERE id=$3 AND user_id=$4`, description, strings.TrimSpace(body.Note), txnID, uid); err != nil {
			internalError(w, r, "update lend transaction", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit lend update", err)
			return
		}
		syncTags(deps.DB, uid, "transaction", txnID, body.Note)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func createLendRepayment(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		lendID, err := intParam(r, "id")
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		var body struct {
			DestinationAccountID int64   `json:"destination_account_id"`
			Amount               float64 `json:"amount"`
			RepaidAt             string  `json:"repaid_at"`
			Note                 string  `json:"note"`
		}
		if err := readJSON(r, &body); err != nil || body.Amount <= 0 {
			errJSON(w, http.StatusBadRequest, "positive amount required")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin lend repayment", err)
			return
		}
		defer tx.Rollback()

		var borrower, sourceAccountType string
		var sourceAccountID, plainID int64
		var principal, repaid float64
		if err := tx.QueryRowContext(ctx, `SELECT l.borrower, l.source_account_id, a.type, l.principal,
			COALESCE((SELECT SUM(amount) FROM fin_lend_repayments WHERE lend_id=l.id),0),
			(SELECT id FROM fin_slates WHERE user_id=l.user_id AND is_plain)
			FROM fin_lends l JOIN fin_accounts a ON a.id=l.source_account_id
			WHERE l.id=$1 AND l.user_id=$2 FOR UPDATE`, lendID, uid,
		).Scan(&borrower, &sourceAccountID, &sourceAccountType, &principal, &repaid, &plainID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				errJSON(w, http.StatusNotFound, "lend not found")
			} else {
				internalError(w, r, "find lend", err)
			}
			return
		}
		if body.DestinationAccountID == 0 {
			body.DestinationAccountID = sourceAccountID
			if sourceAccountType == "credit_card" {
				// Money lent from a card remains payable on that card. The
				// borrower's return normally lands in cash, so prefer a real
				// receiving account instead of silently crediting the card.
				tx.QueryRowContext(ctx, `SELECT id FROM fin_accounts
					WHERE user_id=$1 AND NOT archived AND type<>'credit_card'
					ORDER BY CASE type WHEN 'salary' THEN 0 WHEN 'savings' THEN 1 WHEN 'cash' THEN 2 ELSE 3 END, id
					LIMIT 1`, uid).Scan(&body.DestinationAccountID)
			}
		}
		if err := requireOwnedFinanceRef(ctx, tx, "fin_accounts", uid, body.DestinationAccountID); err != nil {
			errJSON(w, http.StatusNotFound, "destination account not found")
			return
		}
		outstanding := roundMoney(principal - repaid)
		amount := roundMoney(body.Amount)
		if amount > outstanding {
			errJSON(w, http.StatusBadRequest, fmt.Sprintf("repayment exceeds outstanding amount %.2f", outstanding))
			return
		}
		repaidAt := resolveTxnAt(body.RepaidAt, userNow(deps.DB, uid))
		description := "Repayment from " + borrower
		var txnID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions
			(user_id, account_id, type, amount, description, note, txn_at, slate_id)
			VALUES ($1,$2,'lend_repayment',$3,$4,$5,$6,$7) RETURNING id`, uid,
			body.DestinationAccountID, amount, description, strings.TrimSpace(body.Note), repaidAt, plainID,
		).Scan(&txnID); err != nil {
			internalError(w, r, "create repayment transaction", err)
			return
		}
		var repaymentID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_lend_repayments
			(user_id, lend_id, destination_account_id, transaction_id, amount, repaid_at, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, uid, lendID,
			body.DestinationAccountID, txnID, amount, repaidAt, strings.TrimSpace(body.Note),
		).Scan(&repaymentID); err != nil {
			internalError(w, r, "create lend repayment", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit lend repayment", err)
			return
		}
		syncTags(deps.DB, uid, "transaction", txnID, body.Note)
		writeJSON(w, http.StatusCreated, map[string]int64{"id": repaymentID, "transaction_id": txnID})
	}
}

func deleteLendRepayment(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		lendID, err1 := intParam(r, "id")
		repaymentID, err2 := intParam(r, "repaymentID")
		if err1 != nil || err2 != nil {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		ctx := r.Context()
		tx, err := deps.DB.BeginTx(ctx, nil)
		if err != nil {
			internalError(w, r, "begin repayment delete", err)
			return
		}
		defer tx.Rollback()
		var txnID int64
		if err := tx.QueryRowContext(ctx, `DELETE FROM fin_lend_repayments
			WHERE id=$1 AND lend_id=$2 AND user_id=$3 RETURNING transaction_id`, repaymentID, lendID, uid).Scan(&txnID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				errJSON(w, http.StatusNotFound, "repayment not found")
			} else {
				internalError(w, r, "delete repayment", err)
			}
			return
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_transactions WHERE id=$1 AND user_id=$2`, txnID, uid); err != nil {
			internalError(w, r, "delete repayment transaction", err)
			return
		}
		if err := tx.Commit(); err != nil {
			internalError(w, r, "commit repayment delete", err)
			return
		}
		deps.DB.ExecContext(ctx, `DELETE FROM tags WHERE user_id=$1 AND entity_type='transaction' AND entity_id=$2`, uid, txnID)
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
		if err := tx.QueryRowContext(ctx, `SELECT source_transaction_id FROM fin_lends WHERE id=$1 AND user_id=$2 FOR UPDATE`, lendID, uid).Scan(&sourceTxnID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				errJSON(w, http.StatusNotFound, "lend not found")
			} else {
				internalError(w, r, "find lend", err)
			}
			return
		}
		rows, err := tx.QueryContext(ctx, `SELECT transaction_id FROM fin_lend_repayments WHERE lend_id=$1 AND user_id=$2`, lendID, uid)
		if err != nil {
			internalError(w, r, "list repayment transactions", err)
			return
		}
		txnIDs := []int64{sourceTxnID}
		for rows.Next() {
			var txnID int64
			if rows.Scan(&txnID) == nil {
				txnIDs = append(txnIDs, txnID)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			internalError(w, r, "read repayment transactions", err)
			return
		}
		rows.Close()
		if _, err := tx.ExecContext(ctx, `DELETE FROM fin_lend_repayments WHERE lend_id=$1 AND user_id=$2`, lendID, uid); err != nil {
			internalError(w, r, "delete lend repayments", err)
			return
		}
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
	rows, err := d.QueryContext(ctx, `SELECT l.id, l.borrower, l.description,
		GREATEST(l.principal - COALESCE(SUM(r.amount),0),0) AS outstanding
		FROM fin_lends l LEFT JOIN fin_lend_repayments r ON r.lend_id=l.id AND r.user_id=l.user_id
		WHERE l.user_id=$1 GROUP BY l.id HAVING GREATEST(l.principal - COALESCE(SUM(r.amount),0),0) > 0
		ORDER BY l.lent_at, l.id`, uid)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	total := 0.0
	assets := []lendAsset{}
	for rows.Next() {
		var asset lendAsset
		if err := rows.Scan(&asset.ID, &asset.Borrower, &asset.Description, &asset.Outstanding); err != nil {
			return 0, nil, err
		}
		asset.Outstanding = roundMoney(asset.Outstanding)
		total += asset.Outstanding
		assets = append(assets, asset)
	}
	return roundMoney(total), assets, rows.Err()
}
