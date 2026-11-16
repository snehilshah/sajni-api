package api

import (
	"context"
	"database/sql"

	"sajni/internal/db"
)

type sqlRunner interface {
	Exec(string, ...any) (sql.Result, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// NormalizeRef trims whitespace and lowercases a wiki-link reference for matching.
func NormalizeRef(s string) string { return db.NormalizeRef(s) }

// syncTags deletes old tags for an entity and inserts fresh ones parsed from content.
func syncTags(q sqlRunner, userID string, entityType string, entityID int64, content string) error {
	return db.SyncTags(context.Background(), q, userID, entityType, entityID, content)
}

// syncBacklinks stores raw normalized refs for a source. Resolution happens at read time.
func syncBacklinks(q sqlRunner, userID string, sourceType string, sourceID int64, content string) error {
	return db.SyncBacklinks(context.Background(), q, userID, sourceType, sourceID, content)
}
