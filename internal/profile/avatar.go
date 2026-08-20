package profile

import (
	"context"

	"sajni/internal/db"
)

// RerollAvatar advances the only persisted piece of avatar state. The web app
// combines this revision with the user's opaque id to generate a deterministic
// character; no image data needs to be stored or served by the API.
func RerollAvatar(ctx context.Context, d *db.DB, userID string) (int64, error) {
	var revision int64
	err := d.QueryRowContext(ctx, `
		UPDATE users
		SET avatar_revision = avatar_revision + 1
		WHERE id = $1
		RETURNING avatar_revision
	`, userID).Scan(&revision)
	return revision, err
}
