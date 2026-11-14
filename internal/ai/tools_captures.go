package ai

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sajni/internal/db"
)

// listCapturesTool reads fin_captures, the log the capture endpoint
// (api/captures.go) keeps of every message the phone sent.
func listCapturesTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	days := argInt(args, "days", 7)
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}
	limit := argInt(args, "limit", 30)
	if limit < 1 || limit > 100 {
		limit = 100
	}
	q := `SELECT id, received_at, source, sender_label, COALESCE(text, ''), amount, type, description, outcome, reason, txn_id, suggestion, suggestion_taken IS NOT NULL, account_hint
		FROM fin_captures WHERE user_id = $1 AND created_at > now() - ($2 || ' days')::interval`
	params := []any{uid, strconv.FormatInt(days, 10)}
	if o := strings.ToLower(strings.TrimSpace(argStr(args, "outcome"))); o != "" {
		params = append(params, o)
		q += ` AND outcome = $` + strconv.Itoa(len(params))
	}
	if s := strings.TrimSpace(argStr(args, "search")); s != "" {
		params = append(params, "%"+s+"%")
		n := strconv.Itoa(len(params))
		q += ` AND (text ILIKE $` + n + ` OR sender_label ILIKE $` + n + ` OR description ILIKE $` + n + ` OR amount::text ILIKE $` + n + `)`
	}
	params = append(params, limit)
	q += ` ORDER BY id DESC LIMIT $` + strconv.Itoa(len(params))
	rows, err := d.QueryContext(ctx, q, params...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id                                                           int64
			at                                                           time.Time
			source, sender, text, typ, desc, outcome, reason, suggestion string
			amount                                                       float64
			txn                                                          *int64
			taken                                                        bool
			hint                                                         string
		)
		if err := rows.Scan(&id, &at, &source, &sender, &text, &amount, &typ, &desc, &outcome, &reason, &txn, &suggestion, &taken, &hint); err != nil {
			return nil, nil, err
		}
		row := map[string]any{"at": at.Format(time.RFC3339), "via": sender, "source": source, "outcome": outcome}
		if amount != 0 {
			row["amount"], row["type"], row["title"] = amount, typ, desc
		}
		if reason != "" {
			row["reason"] = reason
		}
		if hint != "" {
			row["account_hint"] = hint
		}
		if txn != nil {
			row["txn_id"] = *txn
		}
		if suggestion != "" {
			row["offered"], row["offer_taken"] = suggestion, taken
		}
		if text != "" {
			if len([]rune(text)) > 300 {
				text = string([]rune(text)[:300]) + "…"
			}
			row["message"] = text
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	ignored := []string{}
	if irows, err := d.QueryContext(ctx, `SELECT label FROM fin_capture_ignores WHERE user_id = $1 ORDER BY created_at`, uid); err == nil {
		defer irows.Close()
		for irows.Next() {
			var l string
			if irows.Scan(&l) == nil {
				ignored = append(ignored, l)
			}
		}
	}
	return map[string]any{"captures": out, "not_tracked": ignored}, nil, nil
}

// setCaptureTrackingTool adds or removes an account in fin_capture_ignores.
func setCaptureTrackingTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	hint := strings.ToLower(strings.TrimSpace(argStr(args, "hint")))
	if hint == "" {
		return nil, nil, fmt.Errorf("hint is required")
	}
	label := strings.TrimSpace(argStr(args, "label"))
	if label == "" {
		label = strings.TrimSpace(argStr(args, "hint"))
	}
	track, _ := args["track"].(bool)
	var err error
	if track {
		_, err = d.ExecContext(ctx, `DELETE FROM fin_capture_ignores WHERE user_id = $1 AND hint = $2`, uid, hint)
	} else {
		_, err = d.ExecContext(ctx, `INSERT INTO fin_capture_ignores (user_id, hint, label) VALUES ($1,$2,$3)
			ON CONFLICT (user_id, hint) DO UPDATE SET label = EXCLUDED.label`, uid, hint, label)
	}
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"hint": hint, "tracked": track}, map[string]any{"kind": "capture_tracking", "route": "/finance/transactions"}, nil
}
