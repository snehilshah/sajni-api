package db

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

type tagExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

var (
	tagRe       = regexp.MustCompile(`(?:^|[^\w&])#([\p{L}\p{N}_][\p{L}\p{N}_\-/]*)`)
	codeFenceRe = regexp.MustCompile("(?s)```[\\s\\S]*?```")
	codeSpanRe  = regexp.MustCompile("`[^`\\n]+`")
	urlRe       = regexp.MustCompile(`https?://\S+`)
)

func stripNonProse(content string) string {
	out := codeFenceRe.ReplaceAllString(content, "")
	out = codeSpanRe.ReplaceAllString(out, "")
	out = urlRe.ReplaceAllString(out, "")
	return out
}

// SyncTags deletes old tags for an entity and inserts fresh ones parsed from content.
func (d *DB) SyncTags(ctx context.Context, userID string, entityType string, entityID int64, content string) error {
	return SyncTags(ctx, d, userID, entityType, entityID, content)
}

func SyncTags(ctx context.Context, q tagExecer, userID string, entityType string, entityID int64, content string) error {
	if _, err := q.ExecContext(ctx, "DELETE FROM tags WHERE user_id = $1 AND entity_type = $2 AND entity_id = $3", userID, entityType, entityID); err != nil {
		return err
	}
	scan := stripNonProse(content)
	seen := map[string]bool{}
	for _, m := range tagRe.FindAllStringSubmatch(scan, -1) {
		tag := strings.ToLower(strings.Trim(m[1], "-/_"))
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		if _, err := q.ExecContext(
			ctx,
			"INSERT INTO tags (user_id, entity_type, entity_id, tag) VALUES ($1, $2, $3, $4)",
			userID, entityType, entityID, tag,
		); err != nil {
			return err
		}
	}
	return nil
}
