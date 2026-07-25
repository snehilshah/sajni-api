package ai

import (
	"context"
	"fmt"
	"strings"

	"sajni/internal/db"
)

// Slate tools mirror internal/api/slates.go (duplicated queries to avoid an
// import cycle between internal/ai and internal/api).

func listSlatesTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	rows, err := d.QueryContext(ctx, `SELECT s.id, s.name, s.is_plain,
			COUNT(t.id),
			COALESCE(SUM(t.amount) FILTER (WHERE t.type = 'expense'), 0)
		FROM fin_slates s
		LEFT JOIN fin_transactions t ON t.slate_id = s.id
		WHERE s.user_id = $1 AND NOT s.archived
		GROUP BY s.id ORDER BY s.is_plain DESC, s.created_at ASC`, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, count int64
		var name string
		var isPlain bool
		var spend float64
		rows.Scan(&id, &name, &isPlain, &count, &spend)
		out = append(out, map[string]any{
			"id": id, "name": name, "is_plain": isPlain,
			"txn_count": count, "total_spend": spend,
		})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func createSlateTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	name := strings.TrimSpace(argStr(args, "name"))
	if name == "" {
		return nil, nil, fmt.Errorf("missing name")
	}
	var dup int
	d.QueryRowContext(ctx, `SELECT 1 FROM fin_slates WHERE user_id = $1 AND LOWER(name) = LOWER($2) AND NOT archived`,
		uid, name).Scan(&dup)
	if dup == 1 {
		return nil, nil, fmt.Errorf("a slate named %q already exists", name)
	}
	color := argStr(args, "color")
	if color == "" {
		color = "#2D5A4F"
	}
	var id int64
	if err := d.QueryRowContext(ctx, `INSERT INTO fin_slates (user_id, name, color) VALUES ($1,$2,$3) RETURNING id`,
		uid, name, color).Scan(&id); err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "name": name},
		map[string]any{"kind": "slate_created", "id": id, "title": name, "route": "/finance/slates"}, nil
}

// updateSlateTool mirrors api.updateSlate. Plain is where everything lands by
// default, so renaming or retiring it would leave the default nameless.
func updateSlateTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "slate_id", 0)
	if id <= 0 {
		return nil, nil, fmt.Errorf("missing slate_id")
	}
	var isPlain bool
	var name string
	if err := d.QueryRowContext(ctx, `SELECT is_plain, name FROM fin_slates WHERE id = $1 AND user_id = $2`,
		id, uid).Scan(&isPlain, &name); err != nil {
		return nil, nil, fmt.Errorf("slate not found")
	}
	if isPlain {
		return nil, nil, fmt.Errorf("Plain cannot be renamed or archived")
	}

	if n := strings.TrimSpace(argStr(args, "name")); n != "" {
		if _, err := d.ExecContext(ctx, `UPDATE fin_slates SET name = $1 WHERE id = $2 AND user_id = $3`, n, id, uid); err != nil {
			return nil, nil, err
		}
		name = n
	}
	if c := argStr(args, "color"); c != "" {
		if _, err := d.ExecContext(ctx, `UPDATE fin_slates SET color = $1 WHERE id = $2 AND user_id = $3`, c, id, uid); err != nil {
			return nil, nil, err
		}
	}
	if archived, ok := args["archived"].(bool); ok {
		if _, err := d.ExecContext(ctx, `UPDATE fin_slates SET archived = $1 WHERE id = $2 AND user_id = $3`, archived, id, uid); err != nil {
			return nil, nil, err
		}
	}

	// Read the row back rather than echoing the request: an omitted `archived`
	// must not be reported as false.
	var archived bool
	d.QueryRowContext(ctx, `SELECT archived FROM fin_slates WHERE id = $1 AND user_id = $2`, id, uid).Scan(&archived)
	return map[string]any{"id": id, "name": name, "archived": archived},
		map[string]any{"kind": "slate_updated", "id": id, "title": name, "route": "/finance/slates"}, nil
}

// createBudgetTool mirrors api.createBudget. A budget is a lens: the slate list
// is what it reads (empty = Plain only) and the window is optional, because a
// slate-scoped budget is defined by its slate rather than by dates.
func createBudgetTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	name := strings.TrimSpace(argStr(args, "name"))
	if name == "" {
		return nil, nil, fmt.Errorf("missing name")
	}
	total := argFloat(args, "total_amount")
	if total <= 0 {
		return nil, nil, fmt.Errorf("total_amount must be greater than zero")
	}
	start, end := argStr(args, "start_date"), argStr(args, "end_date")
	if (start == "") != (end == "") {
		return nil, nil, fmt.Errorf("give both start_date and end_date, or neither")
	}

	slateIDs := argInt64Slice(args, "slate_ids")
	for _, sid := range slateIDs {
		var ok bool
		d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_slates WHERE id = $1 AND user_id = $2)`, sid, uid).Scan(&ok)
		if !ok {
			return nil, nil, fmt.Errorf("slate %d not found", sid)
		}
	}

	// Caps are optional and soft. A cap on a category the user does not own is a
	// hallucinated id, so refuse rather than silently dropping it.
	type cap struct {
		categoryID int64
		amount     float64
	}
	caps := []cap{}
	if raw, ok := args["caps"].([]any); ok {
		for _, v := range raw {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			cid := argInt(m, "category_id", 0)
			amt := argFloat(m, "amount")
			if cid <= 0 || amt <= 0 {
				continue
			}
			var exists bool
			d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM fin_categories WHERE id = $1 AND user_id = $2)`, cid, uid).Scan(&exists)
			if !exists {
				return nil, nil, fmt.Errorf("category %d not found", cid)
			}
			caps = append(caps, cap{cid, amt})
		}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO fin_budgets (user_id, name, start_date, end_date, total_amount)
		VALUES ($1,$2,NULLIF($3,'')::date,NULLIF($4,'')::date,$5) RETURNING id`,
		uid, name, start, end, total).Scan(&id); err != nil {
		return nil, nil, err
	}
	for _, c := range caps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO fin_budget_items (budget_id, user_id, category_id, amount) VALUES ($1,$2,$3,$4)`,
			id, uid, c.categoryID, c.amount); err != nil {
			return nil, nil, err
		}
	}
	for _, sid := range slateIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO fin_budget_slates (budget_id, user_id, slate_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
			id, uid, sid); err != nil {
			return nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}

	return map[string]any{"id": id, "name": name, "total_amount": total, "slate_ids": slateIDs},
		map[string]any{"kind": "budget_created", "id": id, "title": name, "route": "/finance/budgets"}, nil
}

// moveToSlateTool is the retroactive sweep: the whole point of slates is that
// outliers are recognised after the fact, so bulk reassignment must be a
// first-class operation rather than N separate transaction edits.
func moveToSlateTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	raw, ok := args["transaction_ids"].([]any)
	if !ok || len(raw) == 0 {
		return nil, nil, fmt.Errorf("missing transaction_ids")
	}
	ids := make([]int64, 0, len(raw))
	for _, v := range raw {
		if f, ok := v.(float64); ok && f > 0 {
			ids = append(ids, int64(f))
		}
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("missing transaction_ids")
	}

	target := argInt(args, "slate_id", 0)
	var name string
	if target > 0 {
		if err := d.QueryRowContext(ctx, `SELECT name FROM fin_slates WHERE id = $1 AND user_id = $2 AND NOT archived`,
			target, uid).Scan(&name); err != nil {
			return nil, nil, fmt.Errorf("slate not found")
		}
	} else {
		// 0 or omitted means Plain — the way to pull something back into
		// normal life after it was marked an outlier by mistake.
		if err := d.QueryRowContext(ctx, `SELECT id, name FROM fin_slates WHERE user_id = $1 AND is_plain`, uid).Scan(&target, &name); err != nil {
			return nil, nil, fmt.Errorf("no slate available")
		}
	}

	// Transfer legs share a slate; moving one without the other splits the pair
	// across normal life and an outlier. Mirrors api.moveTransactionsToSlate.
	res, err := d.ExecContext(ctx, `UPDATE fin_transactions SET slate_id = $1, updated_at = NOW()
		WHERE user_id = $3 AND (id = ANY($2) OR transfer_pair = ANY($2))`, target, ids, uid)
	if err != nil {
		return nil, nil, err
	}
	moved, _ := res.RowsAffected()
	return map[string]any{"moved": moved, "slate_id": target, "slate_name": name},
		map[string]any{"kind": "transactions_updated", "id": target, "title": name, "route": "/finance/transactions"}, nil
}
