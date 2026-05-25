package api

import (
	"regexp"
	"strings"

	"sajni/internal/db"
)

var (
	tagRe       = regexp.MustCompile(`(?:^|[^\w&])#([\p{L}\p{N}_][\p{L}\p{N}_\-/]*)`)
	backlinkRe  = regexp.MustCompile(`\[\[([^\]\n|]+)(?:\|[^\]\n]*)?\]\]`)
	codeFenceRe = regexp.MustCompile("(?s)```[\\s\\S]*?```")
	codeSpanRe  = regexp.MustCompile("`[^`\\n]+`")
	urlRe       = regexp.MustCompile(`https?://\S+`)
	dateRe      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

func stripNonProse(content string) string {
	out := codeFenceRe.ReplaceAllString(content, "")
	out = codeSpanRe.ReplaceAllString(out, "")
	out = urlRe.ReplaceAllString(out, "")
	return out
}

// NormalizeRef trims whitespace and lowercases a wiki-link reference for matching.
func NormalizeRef(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// syncTags deletes old tags for an entity and inserts fresh ones parsed from content.
func syncTags(d *db.DB, userID string, entityType string, entityID int64, content string) error {
	if _, err := d.Exec("DELETE FROM tags WHERE user_id = $1 AND entity_type = $2 AND entity_id = $3", userID, entityType, entityID); err != nil {
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
		if _, err := d.Exec(
			"INSERT INTO tags (user_id, entity_type, entity_id, tag) VALUES ($1, $2, $3, $4)",
			userID, entityType, entityID, tag,
		); err != nil {
			return err
		}
	}
	return nil
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
