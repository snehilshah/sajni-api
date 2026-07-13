package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"sajni/internal/db"
	sharednote "sajni/internal/note"
	"sajni/internal/storage"
)

func main() {
	apply := flag.Bool("apply", false, "copy blobs and update note rows")
	flag.Parse()
	ctx := context.Background()
	database, err := db.New(os.Getenv("DATABASE_URL"))
	if err != nil {
		fatal(err)
	}
	defer database.Close()
	store, err := storage.New(ctx)
	if err != nil {
		fatal(err)
	}
	rows, err := database.QueryContext(ctx, `SELECT id, user_id::text, blob_key FROM notes ORDER BY id`)
	if err != nil {
		fatal(fmt.Errorf("list notes: %w", err))
	}
	type item struct {
		id     int64
		userID string
		oldKey string
	}
	var items []item
	for rows.Next() {
		var value item
		if err := rows.Scan(&value.id, &value.userID, &value.oldKey); err != nil {
			rows.Close()
			fatal(fmt.Errorf("scan note: %w", err))
		}
		if value.oldKey != sharednote.BlobKey(value.userID, value.id) {
			items = append(items, value)
		}
	}
	if err := rows.Close(); err != nil {
		fatal(err)
	}
	if err := rows.Err(); err != nil {
		fatal(fmt.Errorf("iterate notes: %w", err))
	}
	for _, value := range items {
		newKey := sharednote.BlobKey(value.userID, value.id)
		fmt.Printf("note %d: %s -> %s\n", value.id, value.oldKey, newKey)
		data, contentType, err := store.Get(ctx, value.oldKey)
		if err != nil {
			fatal(fmt.Errorf("note %d source %q: %w", value.id, value.oldKey, err))
		}
		if !*apply {
			continue
		}
		if err := store.Put(ctx, newKey, data, contentType); err != nil {
			fatal(fmt.Errorf("copy note %d: %w", value.id, err))
		}
		result, err := database.ExecContext(ctx,
			`UPDATE notes SET blob_key=$1 WHERE id=$2 AND user_id=$3 AND blob_key=$4`,
			newKey, value.id, value.userID, value.oldKey)
		if err != nil {
			_ = store.Delete(ctx, newKey)
			fatal(fmt.Errorf("update note %d: %w", value.id, err))
		}
		updated, err := result.RowsAffected()
		if err != nil || updated != 1 {
			_ = store.Delete(ctx, newKey)
			fatal(fmt.Errorf("note %d changed during migration", value.id))
		}
		var references int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes WHERE blob_key=$1`, value.oldKey).Scan(&references); err != nil {
			fatal(fmt.Errorf("count source references: %w", err))
		}
		if references == 0 {
			if err := store.Delete(ctx, value.oldKey); err != nil && err != storage.ErrNotFound {
				fatal(fmt.Errorf("delete old note blob %q: %w", value.oldKey, err))
			}
		}
	}
	fmt.Printf("%d note blobs %s\n", len(items), map[bool]string{true: "migrated", false: "need migration"}[*apply])
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
