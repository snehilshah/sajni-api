package ai

import (
	"context"
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
	q := `SELECT id, received_at, source, sender_label, COALESCE(text, ''), amount, type, description, outcome, reason, txn_id
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
			id                                               int64
			at                                               time.Time
			source, sender, text, typ, desc, outcome, reason string
			amount                                           float64
			txn                                              *int64
		)
		if err := rows.Scan(&id, &at, &source, &sender, &text, &amount, &typ, &desc, &outcome, &reason, &txn); err != nil {
			return nil, nil, err
		}
		row := map[string]any{"at": at.Format(time.RFC3339), "via": sender, "source": source, "outcome": outcome}
		if amount != 0 {
			row["amount"], row["type"], row["title"] = amount, typ, desc
		}
		if reason != "" {
			row["reason"] = reason
		}
		if txn != nil {
			row["txn_id"] = *txn
		}
		if text != "" {
			if len([]rune(text)) > 300 {
				text = string([]rune(text)[:300]) + "…"
			}
			row["message"] = text
		}
		out = append(out, row)
	}
	return map[string]any{"captures": out}, nil, rows.Err()
}
