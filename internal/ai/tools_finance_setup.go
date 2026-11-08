package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"sajni/internal/db"
)

// Finance setup: accounts, investments, deleting a transaction, and transfers
// between accounts. They mirror the API's account, investment and
// transaction handlers so a connected app can tidy up a month of statements.

var accountTypes = []string{"savings", "salary", "credit_card", "investment", "cash"}

func financeSetupTools(d *db.DB) []Tool {
	acctProps := func() map[string]*genai.Schema {
		return map[string]*genai.Schema{
			"name":            str("Account name, e.g. 'ICICI Savings', 'Slice card'."),
			"type":            str("'savings' | 'salary' | 'credit_card' | 'investment' (a broker or trading account) | 'cash' (cash, wallets, UPI Lite)."),
			"institution":     str("Bank or provider, e.g. 'ICICI'."),
			"opening_balance": num("Balance on the day tracking starts in Sajni. A credit card's is what was owed, as a negative number."),
			"credit_limit":    num("Credit card only."),
			"statement_day":   intg("Credit card only: day of the month the statement is made (1-31)."),
			"due_day":         intg("Credit card only: day of the month the bill is due (1-31)."),
		}
	}
	createAcct := acctProps()
	updateAcct := acctProps()
	updateAcct["account_id"] = intg("Required. The account (list_finance_accounts).")
	updateAcct["archived"] = boolean("true hides the account from pickers; its history stays.")

	return []Tool{
		{
			Name:        "create_finance_account",
			Description: "Add a finance account (bank, card, wallet, broker). Its balance is the opening balance plus everything recorded on it.",
			Mutating:    true,
			Schema:      obj(createAcct, "name"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return saveAccountTool(ctx, d, uid, 0, args)
			},
		},
		{
			Name:        "update_finance_account",
			Description: "Change a finance account: name, type, institution, opening balance (to line its balance up with a statement), card limit and days, or archive it. Only the fields given change.",
			Mutating:    true,
			Schema:      obj(updateAcct, "account_id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "account_id", 0)
				if id == 0 {
					return nil, nil, fmt.Errorf("missing account_id")
				}
				return saveAccountTool(ctx, d, uid, id, args)
			},
		},
		{
			Name:        "delete_transaction",
			Description: "Delete a finance transaction by id, e.g. a duplicate. A transfer goes with its other leg; a payment into an investment comes off its invested amount. Lends, repayments and adjustments can't be deleted here (use the lend tools), nor a bill split with someone until the split is removed.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id": intg("Required. Transaction id from list_finance_transactions."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return deleteTxnTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "create_investment",
			Description: "Add an investment (an FD, RD, SIP, IPO shares, anything held). Money paid into it from an account is then recorded with create_transaction type 'transfer' and to_investment_id, which raises invested_amount; give invested_amount here only for money put in before it was tracked. Turn on auto-debit with set_investment_auto_debit.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"name":            str("Required, e.g. 'JPL IPO', 'HDFC FD'."),
				"type":            str("'sip' | 'rd' | 'fd' | 'other' (shares, IPOs, gold, anything else). Default 'other'."),
				"account_id":      intg("Optional: the account it is paid from."),
				"invested_amount": num("Optional: money already in it before tracking. Default 0."),
				"current_value":   num("Optional: what it is worth now. Defaults to invested_amount."),
				"monthly_amount":  num("Optional: per-cycle amount for an SIP or RD."),
				"frequency":       str("'monthly' | 'quarterly' | 'yearly' | 'lumpsum'. Default monthly for sip/rd, else lumpsum."),
				"start_date":      str("Optional ISO date."),
				"maturity_date":   str("Optional ISO date."),
				"expected_return": num("Optional yearly return in percent (FD/RD interest rate)."),
				"notes":           str("Optional."),
			}, "name"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return createInvestmentTool(ctx, d, uid, args)
			},
		},
		{
			Name:        "update_investment",
			Description: "Change an investment's name, current value (e.g. today's market value), maturity date, expected return, per-cycle amount or notes. Its invested amount follows the transfers into it and isn't set here.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"investment_id":   intg("Required, from list_investments."),
				"name":            str("Optional."),
				"current_value":   num("Optional."),
				"maturity_date":   str("Optional ISO date."),
				"expected_return": num("Optional yearly return in percent."),
				"monthly_amount":  num("Optional per-cycle amount."),
				"notes":           str("Optional."),
			}, "investment_id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return updateInvestmentTool(ctx, d, uid, args)
			},
		},
	}
}

func saveAccountTool(ctx context.Context, d *db.DB, uid string, id int64, args map[string]any) (any, map[string]any, error) {
	has := func(k string) bool { v, ok := args[k]; return ok && v != nil }
	sets, vals := []string{}, []any{}
	add := func(col string, v any) {
		vals = append(vals, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(vals)))
	}
	name := strings.TrimSpace(argStr(args, "name"))
	if has("name") {
		if name == "" {
			return nil, nil, fmt.Errorf("name can't be blank")
		}
		add("name", name)
	} else if id == 0 {
		return nil, nil, fmt.Errorf("name is required")
	}
	if has("type") {
		t := argStr(args, "type")
		ok := false
		for _, at := range accountTypes {
			ok = ok || at == t
		}
		if !ok {
			return nil, nil, fmt.Errorf("type must be one of %s", strings.Join(accountTypes, ", "))
		}
		add("type", t)
	}
	if has("institution") {
		add("institution", strings.TrimSpace(argStr(args, "institution")))
	}
	if has("opening_balance") {
		add("opening_balance", argFloat(args, "opening_balance"))
	}
	if has("credit_limit") {
		add("credit_limit", argFloat(args, "credit_limit"))
	}
	for _, k := range []string{"statement_day", "due_day"} {
		if has(k) {
			day := argInt(args, k, 0)
			if day < 1 || day > 31 {
				return nil, nil, fmt.Errorf("%s must be 1-31", k)
			}
			add(k, day)
		}
	}
	if id > 0 && has("archived") {
		add("archived", argBool(args, "archived", false))
	}

	if id == 0 {
		cols := []string{"user_id"}
		ph := []string{"$1"}
		insVals := []any{uid}
		for i, s := range sets {
			cols = append(cols, strings.SplitN(s, " = ", 2)[0])
			ph = append(ph, fmt.Sprintf("$%d", i+2))
			insVals = append(insVals, vals[i])
		}
		if err := d.QueryRowContext(ctx, `INSERT INTO fin_accounts (`+strings.Join(cols, ", ")+`) VALUES (`+strings.Join(ph, ", ")+`) RETURNING id`,
			insVals...).Scan(&id); err != nil {
			return nil, nil, err
		}
		return map[string]any{"id": id, "name": name},
			map[string]any{"kind": "account_created", "id": id, "title": name, "route": "/finance/accounts"}, nil
	}
	if len(sets) == 0 {
		return nil, nil, fmt.Errorf("nothing to update")
	}
	vals = append(vals, id, uid)
	res, err := d.ExecContext(ctx, "UPDATE fin_accounts SET "+strings.Join(sets, ", ")+
		fmt.Sprintf(", updated_at = NOW() WHERE id = $%d AND user_id = $%d", len(vals)-1, len(vals)), vals...)
	if err != nil {
		return nil, nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil, fmt.Errorf("account not found")
	}
	d.QueryRowContext(ctx, `SELECT name FROM fin_accounts WHERE id = $1 AND user_id = $2`, id, uid).Scan(&name)
	return map[string]any{"id": id, "updated": true},
		map[string]any{"kind": "account_updated", "id": id, "title": name, "route": "/finance/accounts"}, nil
}

func deleteTxnTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing id")
	}
	var desc string
	d.QueryRowContext(ctx, `SELECT description FROM fin_transactions WHERE id = $1 AND user_id = $2`, id, uid).Scan(&desc)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err := db.DeleteTransaction(ctx, tx, uid, id); err != nil {
		if errors.Is(err, db.ErrTxnNotFound) {
			return nil, nil, fmt.Errorf("transaction not found")
		}
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "deleted": true},
		map[string]any{"kind": "transaction_deleted", "id": id, "title": desc, "route": "/finance/transactions"}, nil
}

// transferBetweenAccountsTool is create_transaction's type 'transfer' with
// to_account_id: a transfer_out leg on the paying account and a transfer_in
// leg on the other, paired, as the API's create does.
func transferBetweenAccountsTool(ctx context.Context, d *db.DB, uid string, from int64, amount float64, desc string, txnAt time.Time, args map[string]any) (any, map[string]any, error) {
	to := argInt(args, "to_account_id", 0)
	if to == from {
		return nil, nil, fmt.Errorf("a transfer needs two different accounts")
	}
	var owned int
	d.QueryRowContext(ctx, `SELECT COUNT(*) FROM fin_accounts WHERE user_id = $1 AND id IN ($2, $3)`, uid, from, to).Scan(&owned)
	if owned != 2 {
		return nil, nil, fmt.Errorf("account not found")
	}
	var slate int64
	if err := d.QueryRowContext(ctx, `SELECT id FROM fin_slates WHERE user_id = $1 AND is_plain`, uid).Scan(&slate); err != nil {
		return nil, nil, fmt.Errorf("no slate available")
	}
	if p := argInt(args, "slate_id", 0); p > 0 {
		var okSlate bool
		d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_slates WHERE id = $1 AND user_id = $2 AND NOT archived)`, p, uid).Scan(&okSlate)
		if !okSlate {
			return nil, nil, fmt.Errorf("slate not found")
		}
		slate = p
	}
	note := strings.TrimSpace(argStr(args, "note"))
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var outID, inID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, note, txn_at, linked_account, slate_id)
		VALUES ($1,$2,'transfer_out',$3,$4,$5,$6,$7,$8) RETURNING id`, uid, from, amount, desc, note, txnAt, to, slate).Scan(&outID); err != nil {
		return nil, nil, err
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, note, txn_at, linked_account, transfer_pair, slate_id)
		VALUES ($1,$2,'transfer_in',$3,$4,$5,$6,$7,$8,$9) RETURNING id`, uid, to, amount, desc, note, txnAt, from, outID, slate).Scan(&inID); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fin_transactions SET transfer_pair = $1 WHERE id = $2 AND user_id = $3`, inID, outID, uid); err != nil {
		return nil, nil, err
	}
	for _, id := range []int64{outID, inID} {
		if err := db.SyncTags(ctx, tx, uid, "transaction", id, note); err != nil {
			return nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": outID, "pair_id": inID, "amount": amount, "type": "transfer_out", "to_account_id": to},
		map[string]any{"kind": "transaction_created", "id": outID, "title": fmt.Sprintf("transfer %.2f", amount), "route": "/finance/transactions"}, nil
}

func optDate(args map[string]any, k string) (any, error) {
	s := strings.TrimSpace(argStr(args, k))
	if s == "" {
		return nil, nil
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return nil, fmt.Errorf("%s must be YYYY-MM-DD", k)
	}
	return s, nil
}

func createInvestmentTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	name := strings.TrimSpace(argStr(args, "name"))
	if name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	typ := argStr(args, "type")
	if typ == "" {
		typ = "other"
	}
	switch typ {
	case "sip", "rd", "fd", "other":
	default:
		return nil, nil, fmt.Errorf("type must be sip, rd, fd or other")
	}
	freq := argStr(args, "frequency")
	if freq == "" {
		freq = "lumpsum"
		if typ == "sip" || typ == "rd" {
			freq = "monthly"
		}
	}
	switch freq {
	case "monthly", "quarterly", "yearly", "lumpsum":
	default:
		return nil, nil, fmt.Errorf("frequency must be monthly, quarterly, yearly or lumpsum")
	}
	var acct any
	if a := argInt(args, "account_id", 0); a > 0 {
		var ok bool
		d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_accounts WHERE id = $1 AND user_id = $2)`, a, uid).Scan(&ok)
		if !ok {
			return nil, nil, fmt.Errorf("account not found")
		}
		acct = a
	}
	start, err := optDate(args, "start_date")
	if err != nil {
		return nil, nil, err
	}
	maturity, err := optDate(args, "maturity_date")
	if err != nil {
		return nil, nil, err
	}
	invested := argFloat(args, "invested_amount")
	current := argFloat(args, "current_value")
	if current == 0 {
		current = invested
	}
	if invested < 0 || current < 0 || argFloat(args, "monthly_amount") < 0 {
		return nil, nil, fmt.Errorf("amounts can't be negative")
	}
	var id int64
	if err := d.QueryRowContext(ctx, `INSERT INTO fin_investments
		(user_id, name, type, account_id, invested_amount, current_value, monthly_amount, frequency, start_date, maturity_date, expected_return, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		uid, name, typ, acct, invested, current, argFloat(args, "monthly_amount"), freq, start, maturity,
		argFloat(args, "expected_return"), strings.TrimSpace(argStr(args, "notes"))).Scan(&id); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "name": name, "type": typ},
		map[string]any{"kind": "investment_created", "id": id, "title": name, "route": "/finance/investments"}, nil
}

func updateInvestmentTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "investment_id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing investment_id")
	}
	has := func(k string) bool { v, ok := args[k]; return ok && v != nil }
	sets, vals := []string{}, []any{}
	add := func(col string, v any) {
		vals = append(vals, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(vals)))
	}
	if has("name") {
		n := strings.TrimSpace(argStr(args, "name"))
		if n == "" {
			return nil, nil, fmt.Errorf("name can't be blank")
		}
		add("name", n)
	}
	for _, k := range []string{"current_value", "monthly_amount"} {
		if has(k) {
			if argFloat(args, k) < 0 {
				return nil, nil, fmt.Errorf("%s can't be negative", k)
			}
			add(k, argFloat(args, k))
		}
	}
	if has("expected_return") {
		add("expected_return", argFloat(args, "expected_return"))
	}
	if has("maturity_date") {
		m, err := optDate(args, "maturity_date")
		if err != nil {
			return nil, nil, err
		}
		add("maturity_date", m)
	}
	if has("notes") {
		add("notes", strings.TrimSpace(argStr(args, "notes")))
	}
	if len(sets) == 0 {
		return nil, nil, fmt.Errorf("nothing to update")
	}
	vals = append(vals, id, uid)
	res, err := d.ExecContext(ctx, "UPDATE fin_investments SET "+strings.Join(sets, ", ")+
		fmt.Sprintf(", last_updated = NOW() WHERE id = $%d AND user_id = $%d", len(vals)-1, len(vals)), vals...)
	if err != nil {
		return nil, nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil, fmt.Errorf("investment not found")
	}
	var name string
	d.QueryRowContext(ctx, `SELECT name FROM fin_investments WHERE id = $1 AND user_id = $2`, id, uid).Scan(&name)
	return map[string]any{"id": id, "updated": true},
		map[string]any{"kind": "investment_updated", "id": id, "title": name, "route": "/finance/investments"}, nil
}
