package db

import (
	"context"
	"regexp"
	"strings"
)

var backlinkRe = regexp.MustCompile(`\[\[([^\]\n|]+)(?:\|[^\]\n]*)?\]\]`)

// NormalizeRef trims whitespace and lowercases a wiki-link reference for matching.
func NormalizeRef(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// SyncBacklinks stores the raw normalized [[refs]] of a source; they are
// resolved at read time.
func SyncBacklinks(ctx context.Context, q tagExecer, userID, sourceType string, sourceID int64, content string) error {
	if _, err := q.ExecContext(ctx, "DELETE FROM backlinks WHERE user_id = $1 AND source_type = $2 AND source_id = $3", userID, sourceType, sourceID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range backlinkRe.FindAllStringSubmatch(content, -1) {
		ref := NormalizeRef(m[1])
		// [[task:N|Title]] is a task chip, not a link target.
		if ref == "" || seen[ref] || strings.HasPrefix(ref, "task:") {
			continue
		}
		seen[ref] = true
		if _, err := q.ExecContext(ctx,
			"INSERT INTO backlinks (user_id, source_type, source_id, target_ref) VALUES ($1, $2, $3, $4)",
			userID, sourceType, sourceID, ref,
		); err != nil {
			return err
		}
	}
	return nil
}
