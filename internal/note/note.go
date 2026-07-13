// Package note owns note row and blob creation so HTTP and AI writes share the
// same immutable storage-key contract.
package note

import (
	"context"
	"fmt"

	"sajni/internal/db"
	"sajni/internal/storage"
)

type CreateInput struct {
	UserID      string
	Title       string
	Folder      string
	Description string
	Content     string
}

type Created struct {
	ID      int64
	BlobKey string
}

func BlobKey(userID string, id int64) string {
	return fmt.Sprintf("user_%s/notes/%d.md", userID, id)
}

func Create(ctx context.Context, database *db.DB, store storage.Storage, in CreateInput) (Created, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Created{}, fmt.Errorf("begin note create: %w", err)
	}
	defer tx.Rollback()

	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO notes (user_id, title, blob_key, folder, description) VALUES ($1,$2,'',$3,$4) RETURNING id`,
		in.UserID, in.Title, in.Folder, in.Description).Scan(&id); err != nil {
		return Created{}, fmt.Errorf("insert note: %w", err)
	}
	key := BlobKey(in.UserID, id)
	if err := store.Put(ctx, key, []byte(in.Content), "text/markdown"); err != nil {
		return Created{}, fmt.Errorf("store note %d: %w", id, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = store.Delete(context.Background(), key)
		}
	}()
	if _, err := tx.ExecContext(ctx, `UPDATE notes SET blob_key=$1 WHERE id=$2 AND user_id=$3`, key, id, in.UserID); err != nil {
		return Created{}, fmt.Errorf("set note blob key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Created{}, fmt.Errorf("commit note create: %w", err)
	}
	cleanup = false
	return Created{ID: id, BlobKey: key}, nil
}
