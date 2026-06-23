package api

import (
	"context"
	"regexp"
	"strings"

	"sajni/internal/db"
)

var (
	backlinkRe  = regexp.MustCompile(`\[\[([^\]\n|]+)(?:\|[^\]\n]*)?\]\]`)
	dateRe      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// NormalizeRef trims whitespace and lowercases a wiki-link reference for matching.
func NormalizeRef(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// syncTags deletes old tags for an entity and inserts fresh ones parsed from content.
func syncTags(d *db.DB, userID string, entityType string, entityID int64, content string) error {
	return d.SyncTags(context.Background(), userID, entityType, entityID, content)
}

// syncBacklinks stores raw normalized refs for a source. Resolution happens at read time.
func syncBacklinks(d *db.DB, userID string, sourceType string, sourceID int64, content string) error {
	if _, err := d.Exec("DELETE FROM backlinks WHERE user_id = $1 AND source_type = $2 AND source_id = $3", userID, sourceType, sourceID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range backlinkRe.FindAllStringSubmatch(content, -1) {
		ref := NormalizeRef(m[1])
		if ref == "" || seen[ref] {
			continue
		}
		// `[[task:NN]]` is the TaskChip serialization — it's NOT a
		// wikilink target, so don't store it as a backlink (otherwise the
		// target_ref index fills up with task pointers that no resolver
		// knows how to render).
		if strings.HasPrefix(ref, "task:") {
			continue
		}
		seen[ref] = true
		if _, err := d.Exec(
			"INSERT INTO backlinks (user_id, source_type, source_id, target_ref) VALUES ($1, $2, $3, $4)",
			userID, sourceType, sourceID, ref,
		); err != nil {
			return err
		}
	}
	return nil
}
