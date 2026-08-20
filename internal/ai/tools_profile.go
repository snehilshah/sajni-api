package ai

import (
	"context"

	"sajni/internal/db"
	"sajni/internal/profile"
)

func rerollAvatarTool(ctx context.Context, d *db.DB, userID string) (any, map[string]any, error) {
	revision, err := profile.RerollAvatar(ctx, d, userID)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"avatar_revision": revision}, map[string]any{
		"kind": "avatar_rerolled", "title": "Avatar rerolled", "route": "/settings",
	}, nil
}
