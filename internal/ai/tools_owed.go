package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"sajni/internal/db"
)

// Money the user owes people (db/owed.go), and debts from before Sajni.
func owedTools(d *db.DB) []Tool {
	when := map[string]*genai.Schema{
		"date": str("Optional YYYY-MM-DD (the user's calendar). Defaults to today."),
		"time": str("Optional HH:MM (24-hour, the user's time)."),
	}
	with := func(base map[string]*genai.Schema) map[string]*genai.Schema {
		for k, v := range when {
			base[k] = v
		}
		return base
	}
	return []Tool{
		{
			Name: "list_money_owed",
			Description: "List the people the user owes money to (the reverse of list_lends): per person what the user still owes (debts minus payments back), " +
				"and each debt or payment with its transaction id, date, account and an optional date to pay back by. Money borrowed is not income, and paying it back is not spending. Read-only.",
			Schema: obj(map[string]*genai.Schema{}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return listMoneyOwedTool(ctx, d, uid)
			},
		},
		{
			Name: "borrow",
			Description: "Record money the user owes someone back, e.g. 'Papa sent 1 lakh, I'll return it'. Either mark existing income (transaction_id, from list_finance_transactions type income) " +
				"or record a new one (account_id + amount). It stays in the account but stops counting as income. With no_money_moved it's a debt from before Sajni and touches no account balance.",
			Mutating: true,
			Schema: obj(with(map[string]*genai.Schema{
				"person":         str("Required. Who the user owes."),
				"transaction_id": intg("An existing income transaction to mark. Omit to record a new one."),
				"account_id":     intg("For a new one: the account the money came into (list_accounts). With no_money_moved, any account; its balance doesn't change."),
				"amount":         num("For a new one: the amount."),
				"description":    str("For a new one: a title, e.g. 'From Papa for shares'."),
				"note":           str("Optional note."),
				"due_date":       str("Optional YYYY-MM-DD the user means to pay it back by."),
				"no_money_moved": boolean("True for a debt from before Sajni: nothing comes into any account."),
			}), "person"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return borrowTool(ctx, d, uid, args)
			},
		},
		{
			Name: "pay_back",
			Description: "Record the user paying back money they owe someone: mark an existing expense (transaction_id, from list_finance_transactions type expense) or record a new payment (account_id + amount). " +
				"Money leaves the account but it isn't spending. Can't exceed what the user owes that person (list_money_owed).",
			Mutating: true,
			Schema: obj(with(map[string]*genai.Schema{
				"person":         str("Required. Who is being paid back, as in list_money_owed."),
				"transaction_id": intg("An existing expense to mark. Omit to record a new payment."),
				"account_id":     intg("For a new payment: the account it's paid from."),
				"amount":         num("For a new payment: the amount."),
				"description":    str("For a new payment: a title. Defaults to 'Paid back <person>'."),
				"note":           str("Optional note."),
			}), "person"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return payBackTool(ctx, d, uid, args)
			},
		},
		{
			Name: "net_off",
			Description: "When the user and someone owe each other, close the smaller amount against the bigger one: e.g. Papa owes ₹4,911 and the user owes him ₹2,724.33, so both drop by ₹2,724.33 and Papa still owes ₹2,186.67. " +
				"No money moves. Undo with undo_owed on the net-off.",
			Mutating: true,
			Schema:   obj(map[string]*genai.Schema{"person": str("Required. The person.")}, "person"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return netOffTool(ctx, d, uid, args)
			},
		},
		{
			Name: "undo_owed",
			Description: "Undo a debt or a payment back by its transaction id (list_money_owed): a borrow goes back to income, a payment back to an expense, a debt from before Sajni is deleted, and a net-off is undone on both sides. " +
				"A debt can't be undone while the payments back would then exceed it.",
			Mutating: true,
			Schema:   obj(map[string]*genai.Schema{"transaction_id": intg("Required.")}, "transaction_id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return undoOwedTool(ctx, d, uid, args)
			},
		},
		{
			Name: "lend_before_sajni",
			Description: "Record that someone owed the user money from before they started using Sajni (e.g. 'Alefiya owed me 1,110 from September'). It's a lend that moves no money, so no account balance changes; " +
				"payments back then settle it like any lend (settle_lends).",
			Mutating: true,
			Schema: obj(with(map[string]*genai.Schema{
				"borrower":    str("Required. Who owes the user."),
				"amount":      num("Required."),
				"account_id":  intg("Required. Any of the user's accounts (list_accounts); its balance doesn't change."),
				"description": str("A title. Defaults to 'Owed before Sajni'."),
				"note":        str("Optional note."),
			}), "borrower", "amount", "account_id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				return lendBeforeTool(ctx, d, uid, args)
			},
		},
	}
}

func plainSlate(ctx context.Context, d *db.DB, uid string) (int64, error) {
	var id int64
	err := d.QueryRowContext(ctx, `SELECT id FROM fin_slates WHERE user_id = $1 AND is_plain`, uid).Scan(&id)
	return id, err
}

func ownAccount(ctx context.Context, d *db.DB, uid string, id int64) error {
	var ok bool
	d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_accounts WHERE id = $1 AND user_id = $2)`, id, uid).Scan(&ok)
	if !ok {
		return fmt.Errorf("account %d not found (list_accounts)", id)
	}
	return nil
}

var owedMeta = func(title string) map[string]any {
	return map[string]any{"kind": "lend_updated", "id": 0, "title": title, "route": "/finance/lends"}
}

func listMoneyOwedTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	loc := userTZLoc(ctx, d, uid)
	rows, err := d.QueryContext(ctx, `SELECT o.person, t.id, o.kind, t.type, t.account_id, a.name, t.amount, t.description, t.txn_at, o.due_date::text
		FROM fin_owed o JOIN fin_transactions t ON t.id = o.transaction_id JOIN fin_accounts a ON a.id = t.account_id
		WHERE o.user_id = $1 ORDER BY LOWER(BTRIM(o.person)), t.txn_at`, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type person struct {
		Name    string           `json:"person"`
		YouOwe  float64          `json:"you_owe"`
		Entries []map[string]any `json:"entries"`
	}
	var people []*person
	byKey := map[string]*person{}
	for rows.Next() {
		var name, kind, typ, account, desc string
		var txnID, accountID int64
		var amount float64
		var at time.Time
		var due *string
		if err := rows.Scan(&name, &txnID, &kind, &typ, &accountID, &account, &amount, &desc, &at, &due); err != nil {
			return nil, nil, err
		}
		k := strings.ToLower(strings.TrimSpace(name))
		p := byKey[k]
		if p == nil {
			p = &person{Name: name}
			byKey[k] = p
			people = append(people, p)
		}
		if kind == "debt" {
			p.YouOwe += amount
		} else {
			p.YouOwe -= amount
		}
		e := map[string]any{"transaction_id": txnID, "kind": kind, "type": typ, "account_id": accountID, "account": account,
			"amount": financeToolMoney(amount), "description": desc, "at": at.In(loc).Format("2006-01-02 15:04")}
		if due != nil {
			e["pay_back_by"] = *due
		}
		p.Entries = append(p.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	out := []map[string]any{}
	var total float64
	for _, p := range people {
		p.YouOwe = financeToolMoney(max(p.YouOwe, 0))
		total += p.YouOwe
		out = append(out, map[string]any{"person": p.Name, "you_owe": p.YouOwe, "entries": p.Entries})
	}
	return map[string]any{"people": out, "total_you_owe": financeToolMoney(total)}, nil, nil
}

func borrowTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	person := strings.TrimSpace(argStr(args, "person"))
	var due *string
	if v := strings.TrimSpace(argStr(args, "due_date")); v != "" {
		due = &v
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	id := argInt(args, "transaction_id", 0)
	if id > 0 {
		if err := db.Borrow(ctx, tx, uid, person, id, due); err != nil {
			return nil, nil, err
		}
	} else {
		acct := argInt(args, "account_id", 0)
		if err := ownAccount(ctx, d, uid, acct); err != nil {
			return nil, nil, err
		}
		at, err := argTxnAt(ctx, d, uid, args)
		if err != nil {
			return nil, nil, err
		}
		slate, err := plainSlate(ctx, d, uid)
		if err != nil {
			return nil, nil, err
		}
		id, err = db.AddDebt(ctx, tx, uid, db.OwedDebt{Person: person, AccountID: acct, Amount: argFloat(args, "amount"),
			Description: argStr(args, "description"), Note: argStr(args, "note"), At: at, SlateID: slate, Due: due,
			NoMoneyMoved: argBool(args, "no_money_moved", false)})
		if err != nil {
			return nil, nil, err
		}
	}
	owes, err := db.OwedTo(ctx, tx, uid, person)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"transaction_id": id, "person": person, "you_owe": financeToolMoney(owes)}, owedMeta(person), nil
}

func payBackTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	person := strings.TrimSpace(argStr(args, "person"))
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	id := argInt(args, "transaction_id", 0)
	if id <= 0 {
		acct := argInt(args, "account_id", 0)
		if err := ownAccount(ctx, d, uid, acct); err != nil {
			return nil, nil, err
		}
		amount := argFloat(args, "amount")
		if amount <= 0 {
			return nil, nil, fmt.Errorf("amount must be above zero")
		}
		at, err := argTxnAt(ctx, d, uid, args)
		if err != nil {
			return nil, nil, err
		}
		slate, err := plainSlate(ctx, d, uid)
		if err != nil {
			return nil, nil, err
		}
		desc := strings.TrimSpace(argStr(args, "description"))
		if desc == "" {
			desc = "Paid back " + person
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, note, txn_at, slate_id)
			VALUES ($1,$2,'expense',$3,$4,$5,$6,$7) RETURNING id`, uid, acct, amount, desc, strings.TrimSpace(argStr(args, "note")), at, slate).Scan(&id); err != nil {
			return nil, nil, err
		}
	}
	if err := db.PayBack(ctx, tx, uid, person, id); err != nil {
		return nil, nil, err
	}
	owes, err := db.OwedTo(ctx, tx, uid, person)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"transaction_id": id, "person": person, "you_still_owe": financeToolMoney(owes)}, owedMeta(person), nil
}

func netOffTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	person := strings.TrimSpace(argStr(args, "person"))
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	amount, err := db.NetOff(ctx, tx, uid, person, userTZNow(ctx, d, uid))
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"person": person, "netted": financeToolMoney(amount)}, owedMeta(person), nil
}

func undoOwedTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	txnID := argInt(args, "transaction_id", 0)
	var owedID int64
	var person string
	if err := d.QueryRowContext(ctx, `SELECT id, person FROM fin_owed WHERE transaction_id = $1 AND user_id = $2`, txnID, uid).Scan(&owedID, &person); err != nil {
		// A net-off's lend half: undo through its pair.
		if err := d.QueryRowContext(ctx, `SELECT o.id, o.person FROM fin_transactions t JOIN fin_owed o ON o.transaction_id = t.transfer_pair
			WHERE t.id = $1 AND t.user_id = $2 AND t.type = 'netoff'`, txnID, uid).Scan(&owedID, &person); err != nil {
			return nil, nil, fmt.Errorf("transaction %d isn't a debt or a payment back (list_money_owed)", txnID)
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err := db.Unowe(ctx, tx, uid, owedID); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"undone": true, "person": person}, owedMeta(person), nil
}

func lendBeforeTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	acct := argInt(args, "account_id", 0)
	if err := ownAccount(ctx, d, uid, acct); err != nil {
		return nil, nil, err
	}
	at, err := argTxnAt(ctx, d, uid, args)
	if err != nil {
		return nil, nil, err
	}
	slate, err := plainSlate(ctx, d, uid)
	if err != nil {
		return nil, nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	borrower := strings.TrimSpace(argStr(args, "borrower"))
	id, err := db.AddLendBefore(ctx, tx, uid, borrower, acct, argFloat(args, "amount"), argStr(args, "description"), argStr(args, "note"), at, slate)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"lend_id": id, "borrower": borrower}, owedMeta(borrower), nil
}
