package ai

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"sajni/internal/db"
)

// Pocket tools mirror internal/api/pockets.go (duplicated queries to avoid
// an import cycle between internal/ai and internal/api).

func listPocketsTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	now := userTZNow(ctx, d, uid)
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	from := first.Format("2006-01-02")
	to := first.AddDate(0, 1, -1).Format("2006-01-02")

	rows, err := d.QueryContext(ctx, `SELECT id, name, is_active FROM fin_pockets
		WHERE user_id = $1 AND archived = FALSE AND kind = 'personal' ORDER BY created_at ASC`, uid)
	if err != nil {
		return nil, nil, err
	}
	items := []map[string]any{}
	var activeID any
	for rows.Next() {
		var id int64
		var name string
		var active bool
		rows.Scan(&id, &name, &active)
		if active {
			activeID = id
		}
		items = append(items, map[string]any{"id": id, "name": name, "is_active": active})
	}
	rows.Close()

	var generalSpend float64
	srows, serr := d.QueryContext(ctx, `SELECT pocket_id, COALESCE(SUM(amount),0)
		FROM fin_transactions WHERE user_id = $1 AND type = 'expense'
		AND (txn_at AT TIME ZONE 'Asia/Kolkata')::date BETWEEN $2 AND $3
		GROUP BY pocket_id`, uid, from, to)
	if serr == nil {
		spend := map[int64]float64{}
		for srows.Next() {
			var pid *int64
			var sum float64
			srows.Scan(&pid, &sum)
			if pid == nil {
				generalSpend = sum
			} else {
				spend[*pid] = sum
			}
		}
		srows.Close()
		for _, it := range items {
			it["month_spend"] = spend[it["id"].(int64)]
		}
	}
	// Shared pockets the user owns or belongs to, with their net balance
	// (positive = others owe them). Mirrors api.listSharedPocketSummaries.
	shared := []map[string]any{}
	shrows, sherr := d.QueryContext(ctx, `
		SELECT p.id, p.name, p.user_id = $1,
			(SELECT COUNT(*) FROM fin_pocket_members mm WHERE mm.pocket_id = p.id AND mm.left_at IS NULL),
			COALESCE((SELECT
				COALESCE((SELECT SUM(ROUND(e.amount*100)::bigint) FROM fin_shared_expenses e WHERE e.paid_by = me.id),0)
				- COALESCE((SELECT SUM(ROUND(s.amount*100)::bigint) FROM fin_expense_shares s WHERE s.member_id = me.id),0)
				+ COALESCE((SELECT SUM(ROUND(st.amount*100)::bigint) FROM fin_pocket_settlements st WHERE st.from_member = me.id),0)
				- COALESCE((SELECT SUM(ROUND(st.amount*100)::bigint) FROM fin_pocket_settlements st WHERE st.to_member = me.id),0)
			),0)
		FROM fin_pockets p
		LEFT JOIN fin_pocket_members me ON me.pocket_id = p.id AND me.user_id = $1 AND me.left_at IS NULL
		WHERE p.kind = 'shared' AND p.archived = FALSE AND (p.user_id = $1 OR me.id IS NOT NULL)
		ORDER BY p.created_at ASC`, uid)
	if sherr == nil {
		sharedIdx := map[int64]int{}
		var sharedIDs []int64
		for shrows.Next() {
			var id, memberCount, netPaise int64
			var name string
			var isOwner bool
			shrows.Scan(&id, &name, &isOwner, &memberCount, &netPaise)
			sharedIdx[id] = len(shared)
			sharedIDs = append(sharedIDs, id)
			shared = append(shared, map[string]any{
				"id": id, "name": name, "is_owner": isOwner,
				"member_count": memberCount, "my_balance": float64(netPaise) / 100,
				"members": []map[string]any{},
			})
		}
		shrows.Close()
		if len(sharedIDs) > 0 {
			mrows, merr := d.QueryContext(ctx, `SELECT pocket_id, id, display_name, COALESCE(user_id::text,'') = $2
				FROM fin_pocket_members WHERE pocket_id = ANY($1) AND left_at IS NULL ORDER BY id`, sharedIDs, uid)
			if merr == nil {
				for mrows.Next() {
					var pid, mid int64
					var name string
					var isMe bool
					mrows.Scan(&pid, &mid, &name, &isMe)
					entry := shared[sharedIdx[pid]]
					entry["members"] = append(entry["members"].([]map[string]any),
						map[string]any{"member_id": mid, "name": name, "is_me": isMe})
				}
				mrows.Close()
			}
		}
	}
	return map[string]any{
		"items": items, "count": len(items),
		"general_spend": generalSpend, "active_pocket_id": activeID,
		"shared": shared,
	}, nil, nil
}

func createPocketTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	name := strings.TrimSpace(argStr(args, "name"))
	if name == "" {
		return nil, nil, fmt.Errorf("missing name")
	}
	var dup int
	d.QueryRowContext(ctx, `SELECT 1 FROM fin_pockets WHERE user_id = $1 AND LOWER(name) = LOWER($2) AND NOT archived`,
		uid, name).Scan(&dup)
	if dup == 1 {
		return nil, nil, fmt.Errorf("a pocket named %q already exists", name)
	}
	color := argStr(args, "color")
	if color == "" {
		color = "#2D5A4F"
	}
	var id int64
	if err := d.QueryRowContext(ctx, `INSERT INTO fin_pockets (user_id, name, color) VALUES ($1,$2,$3) RETURNING id`,
		uid, name, color).Scan(&id); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "name": name},
		map[string]any{"kind": "pocket_created", "id": id, "title": name, "route": "/finance"}, nil
}

func setActivePocketTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	pid := argInt(args, "pocket_id", 0)
	tx, err := d.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE fin_pockets SET is_active = FALSE WHERE user_id = $1 AND is_active`, uid); err != nil {
		return nil, nil, err
	}
	name := ""
	if pid > 0 {
		res, err := tx.ExecContext(ctx, `UPDATE fin_pockets SET is_active = TRUE WHERE id = $1 AND user_id = $2 AND NOT archived AND kind = 'personal'`, pid, uid)
		if err != nil {
			return nil, nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, nil, fmt.Errorf("pocket not found")
		}
		tx.QueryRowContext(ctx, `SELECT name FROM fin_pockets WHERE id = $1`, pid).Scan(&name)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	if pid > 0 {
		return map[string]any{"active_pocket_id": pid, "name": name},
			map[string]any{"kind": "pocket_activated", "id": pid, "title": name, "route": "/finance"}, nil
	}
	return map[string]any{"active_pocket_id": nil},
		map[string]any{"kind": "pocket_activated", "id": int64(0), "title": "General", "route": "/finance"}, nil
}

// sharedPocketMember resolves the caller's active member row in a shared
// pocket (mirrors api.requireSharedPocketAccess).
func sharedPocketMember(ctx context.Context, d *db.DB, uid string, pocketID int64) (memberID int64, memberName, pocketName, ownerID string, err error) {
	err = d.QueryRowContext(ctx, `
		SELECT m.id, m.display_name, p.name, p.user_id
		FROM fin_pockets p
		JOIN fin_pocket_members m ON m.pocket_id = p.id AND m.user_id = $2 AND m.left_at IS NULL
		WHERE p.id = $1 AND p.kind = 'shared'`, pocketID, uid).
		Scan(&memberID, &memberName, &pocketName, &ownerID)
	if err != nil {
		err = fmt.Errorf("shared pocket not found (or you're not a member)")
	}
	return
}

func logPocketActivityAI(ctx context.Context, d *db.DB, ownerID string, pocketID int64, actor, kind, detailJSON string) {
	d.ExecContext(ctx, `INSERT INTO fin_pocket_activity (owner_id, pocket_id, actor_name, kind, detail) VALUES ($1,$2,$3,$4,$5)`,
		ownerID, pocketID, actor, kind, detailJSON)
}

// addSharedExpenseTool records an expense the USER paid in a shared pocket,
// split equally over the given participants (default: everyone). If
// account_id is passed the full amount also lands on the user's own ledger
// as an echo txn — never on anyone else's.
func addSharedExpenseTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	pid := argInt(args, "pocket_id", 0)
	amount := argFloat(args, "amount")
	if pid == 0 {
		return nil, nil, fmt.Errorf("missing pocket_id")
	}
	if amount <= 0 {
		return nil, nil, fmt.Errorf("amount must be positive")
	}
	me, meName, pocketName, ownerID, err := sharedPocketMember(ctx, d, uid, pid)
	if err != nil {
		return nil, nil, err
	}
	desc := argStr(args, "description")

	participants := argInt64Slice(args, "participant_member_ids")
	if len(participants) == 0 {
		rows, rerr := d.QueryContext(ctx, `SELECT id FROM fin_pocket_members WHERE pocket_id = $1 AND left_at IS NULL ORDER BY id`, pid)
		if rerr != nil {
			return nil, nil, rerr
		}
		for rows.Next() {
			var mid int64
			rows.Scan(&mid)
			participants = append(participants, mid)
		}
		rows.Close()
	} else {
		// Dedupe before the count check — ANY() matches rows once, so a
		// repeated id from the model would fail the comparison (and would
		// otherwise double a share below).
		slices.Sort(participants)
		participants = slices.Compact(participants)
		var ok bool
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*) = cardinality($2::bigint[])
			FROM fin_pocket_members WHERE pocket_id = $1 AND id = ANY($2) AND left_at IS NULL`,
			pid, participants).Scan(&ok); err != nil || !ok {
			return nil, nil, fmt.Errorf("some participants are not in this pocket")
		}
	}
	if len(participants) == 0 {
		return nil, nil, fmt.Errorf("no participants")
	}

	// Equal split in paise, remainder to the lowest member ids (same rule
	// as the API's resolveShares).
	slices.Sort(participants)
	total := int64(math.Round(amount * 100))
	n := int64(len(participants))
	base, rem := total/n, total%n

	tx, err := d.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	insArgs := []any{ownerID, pid, me, amount, desc, uid}
	spentCol := "NOW()"
	if date := argStr(args, "date"); date != "" {
		spentCol = "($7::timestamp AT TIME ZONE 'Asia/Kolkata')"
		insArgs = append(insArgs, date)
	}
	var eid int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_shared_expenses (owner_id, pocket_id, paid_by, amount, description, split, spent_at, created_by)
		VALUES ($1,$2,$3,$4,$5,'equal',`+spentCol+`,$6) RETURNING id`, insArgs...).Scan(&eid); err != nil {
		return nil, nil, err
	}
	for i, mid := range participants {
		share := base
		if int64(i) < rem {
			share++
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO fin_expense_shares (expense_id, member_id, amount) VALUES ($1,$2,$3)`,
			eid, mid, float64(share)/100); err != nil {
			return nil, nil, err
		}
	}
	var echoID any
	if acct := argInt(args, "account_id", 0); acct > 0 {
		var owned int
		tx.QueryRowContext(ctx, `SELECT 1 FROM fin_accounts WHERE id = $1 AND user_id = $2`, acct, uid).Scan(&owned)
		if owned != 1 {
			return nil, nil, fmt.Errorf("account not found")
		}
		var catArg any
		if catName := strings.TrimSpace(argStr(args, "category_name")); catName != "" {
			if cid := resolveCategoryID(ctx, d, uid, catName); cid > 0 {
				catArg = cid
			}
		}
		var tid int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at, shared_expense_id)
			SELECT $1, $2, $3, 'expense', $4, $5, spent_at, id FROM fin_shared_expenses WHERE id = $6 RETURNING id`,
			uid, acct, catArg, amount, desc, eid).Scan(&tid); err != nil {
			return nil, nil, err
		}
		echoID = tid
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	logPocketActivityAI(ctx, d, ownerID, pid, meName, "expense_added",
		fmt.Sprintf(`{"description":%q,"amount":%.2f}`, desc, amount))
	route := fmt.Sprintf("/finance/pockets/%d", pid)
	return map[string]any{"id": eid, "pocket": pocketName, "amount": amount, "participants": len(participants), "echo_txn_id": echoID},
		map[string]any{"kind": "pocket_expense_added", "id": eid, "title": desc, "route": route}, nil
}

// recordSettlementTool records a settle-up between the user and another
// member. direction i_paid = user paid them back; i_received = they paid
// the user. account_id (optional) echoes the user's own leg to their ledger.
func recordSettlementTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	pid := argInt(args, "pocket_id", 0)
	amount := argFloat(args, "amount")
	other := argInt(args, "counterparty_member_id", 0)
	if pid == 0 || other == 0 {
		return nil, nil, fmt.Errorf("missing pocket_id or counterparty_member_id")
	}
	if amount <= 0 {
		return nil, nil, fmt.Errorf("amount must be positive")
	}
	me, meName, pocketName, ownerID, err := sharedPocketMember(ctx, d, uid, pid)
	if err != nil {
		return nil, nil, err
	}
	if other == me {
		return nil, nil, fmt.Errorf("counterparty must be someone else")
	}
	var otherName string
	if err := d.QueryRowContext(ctx, `SELECT display_name FROM fin_pocket_members WHERE id = $1 AND pocket_id = $2 AND left_at IS NULL`,
		other, pid).Scan(&otherName); err != nil {
		return nil, nil, fmt.Errorf("counterparty not found in this pocket")
	}
	from, to := me, other
	txnType, desc := "expense", "Settlement to "+otherName
	if argStr(args, "direction") == "i_received" {
		from, to = other, me
		txnType, desc = "income", "Settlement from "+otherName
	}

	tx, err := d.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	insArgs := []any{ownerID, pid, from, to, amount, uid}
	settledCol := "NOW()"
	if date := argStr(args, "date"); date != "" {
		settledCol = "($7::timestamp AT TIME ZONE 'Asia/Kolkata')"
		insArgs = append(insArgs, date)
	}
	var sid int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_pocket_settlements (owner_id, pocket_id, from_member, to_member, amount, settled_at, created_by)
		VALUES ($1,$2,$3,$4,$5,`+settledCol+`,$6) RETURNING id`, insArgs...).Scan(&sid); err != nil {
		return nil, nil, err
	}
	var echoID any
	if acct := argInt(args, "account_id", 0); acct > 0 {
		var owned int
		tx.QueryRowContext(ctx, `SELECT 1 FROM fin_accounts WHERE id = $1 AND user_id = $2`, acct, uid).Scan(&owned)
		if owned != 1 {
			return nil, nil, fmt.Errorf("account not found")
		}
		var tid int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, settlement_id)
			SELECT $1, $2, $3, $4, $5, settled_at, id FROM fin_pocket_settlements WHERE id = $6 RETURNING id`,
			uid, acct, txnType, amount, desc+" · "+pocketName, sid).Scan(&tid); err != nil {
			return nil, nil, err
		}
		echoID = tid
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	var fromName, toName string = meName, otherName
	if from == other {
		fromName, toName = otherName, meName
	}
	logPocketActivityAI(ctx, d, ownerID, pid, meName, "settlement_added",
		fmt.Sprintf(`{"from":%q,"to":%q,"amount":%.2f}`, fromName, toName, amount))
	route := fmt.Sprintf("/finance/pockets/%d", pid)
	return map[string]any{"id": sid, "pocket": pocketName, "from": fromName, "to": toName, "amount": amount, "echo_txn_id": echoID},
		map[string]any{"kind": "pocket_settlement_added", "id": sid, "title": fromName + " → " + toName, "route": route}, nil
}

// setInvestmentAutoDebitTool mirrors the validation in api.updateInvestment.
func setInvestmentAutoDebitTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "investment_id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing investment_id")
	}
	enabled := argBool(args, "enabled", false)

	var name, freq string
	var monthly float64
	var acct *int64
	var start *string
	if err := d.QueryRowContext(ctx, `SELECT name, account_id, monthly_amount, frequency, start_date::text
		FROM fin_investments WHERE id = $1 AND user_id = $2`, id, uid).Scan(&name, &acct, &monthly, &freq, &start); err != nil {
		return nil, nil, fmt.Errorf("investment not found")
	}
	if a := argInt(args, "account_id", 0); a > 0 {
		acct = &a
		d.ExecContext(ctx, `UPDATE fin_investments SET account_id = $1 WHERE id = $2 AND user_id = $3`, a, id, uid)
	}

	if !enabled {
		d.ExecContext(ctx, `UPDATE fin_investments SET auto_debit = FALSE, next_debit_date = NULL, last_updated = NOW()
			WHERE id = $1 AND user_id = $2`, id, uid)
		return map[string]any{"id": id, "auto_debit": false},
			map[string]any{"kind": "investment_updated", "id": id, "title": name, "route": "/finance/investments"}, nil
	}

	if acct == nil {
		return nil, nil, fmt.Errorf("auto-debit needs a linked account")
	}
	if monthly <= 0 {
		return nil, nil, fmt.Errorf("auto-debit needs a per-cycle amount on the investment")
	}
	switch freq {
	case "monthly", "quarterly", "yearly":
	default:
		return nil, nil, fmt.Errorf("auto-debit needs a recurring frequency (monthly/quarterly/yearly), got %q", freq)
	}
	next := argStr(args, "next_debit_date")
	if next == "" {
		// Project start_date's day-of-month to the next occurrence >= today.
		now := userTZNow(ctx, d, uid)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		next = today.Format("2006-01-02")
		if start != nil && *start != "" {
			if s, err := time.Parse("2006-01-02", *start); err == nil {
				if !s.Before(today) {
					next = s.Format("2006-01-02")
				} else {
					anchor := s.Day()
					cand := anchoredMonthDate(today.Year(), today.Month(), anchor)
					if cand.Before(today) {
						nextMonth := time.Date(today.Year(), today.Month()+1, 1, 0, 0, 0, 0, time.UTC)
						cand = anchoredMonthDate(nextMonth.Year(), nextMonth.Month(), anchor)
					}
					next = cand.Format("2006-01-02")
				}
			}
		}
	}
	nextDate, err := time.Parse("2006-01-02", next)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid next_debit_date")
	}
	if _, err := d.ExecContext(ctx, `UPDATE fin_investments SET auto_debit = TRUE, next_debit_date = $1, anchor_day = $2, last_updated = NOW()
		WHERE id = $3 AND user_id = $4`, next, nextDate.Day(), id, uid); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "auto_debit": true, "next_debit_date": next},
		map[string]any{"kind": "investment_updated", "id": id, "title": name, "route": "/finance/investments"}, nil
}

func anchoredMonthDate(year int, month time.Month, day int) time.Time {
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > last {
		day = last
	}
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
