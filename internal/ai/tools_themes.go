package ai

import (
	"context"
	"encoding/json"
	"fmt"

	"sajni/internal/db"
	"sajni/internal/theme"
)

func generateThemeTool(ctx context.Context, s *Service, uid string, args map[string]any) (any, map[string]any, error) {
	prompt := argStr(args, "prompt")
	if prompt == "" {
		return nil, nil, fmt.Errorf("missing prompt")
	}

	t, err := theme.Generate(ctx, s, s.db, uid, prompt)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{
			"id":        t.ID,
			"name":      t.Name,
			"seeds":     t.Seeds,
			"is_active": t.IsActive,
		},
		map[string]any{"kind": "theme_created", "id": t.ID, "title": t.Name, "route": "/settings#themes"}, nil
}

func listThemesTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, name, seeds, is_active, created_at::text
		FROM user_themes WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, created string
		var seedsRaw []byte
		var active bool
		if err := rows.Scan(&id, &name, &seedsRaw, &active, &created); err != nil {
			return nil, nil, err
		}
		var seeds theme.Seeds
		if err := json.Unmarshal(seedsRaw, &seeds); err != nil {
			return nil, nil, err
		}
		out = append(out, map[string]any{
			"id": id, "name": name, "seeds": seeds,
			"is_active": active, "created_at": created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func activateThemeTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing id")
	}
	if err := theme.Activate(ctx, d, uid, id); err != nil {
		return nil, nil, err
	}
	t, err := theme.Load(ctx, d, uid, id)
	if err != nil {
		return map[string]any{"id": id, "is_active": true}, nil, nil
	}
	return map[string]any{"id": t.ID, "name": t.Name, "is_active": true},
		map[string]any{"kind": "theme_activated", "id": t.ID, "title": t.Name, "route": "/settings#themes"}, nil
}
