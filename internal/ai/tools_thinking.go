package ai

import (
	"context"
	"encoding/json"
	"fmt"

	"sajni/internal/db"
	"sajni/internal/thinking"
)

func listThinkingProjectsTool(ctx context.Context, d *db.DB, uid string) (any, map[string]any, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT p.id, p.title, p.description, COALESCE(p.thesis,''),
		       COALESCE(c.cnt,0)::int, p.updated_at::text
		FROM thinking_projects p
		LEFT JOIN (SELECT project_id, COUNT(*) cnt FROM thinking_cards GROUP BY project_id) c
		  ON c.project_id = p.id
		WHERE p.user_id=$1
		ORDER BY p.updated_at DESC LIMIT 50`, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var title, desc, thesis, updated string
		var cnt int
		rows.Scan(&id, &title, &desc, &thesis, &cnt, &updated)
		out = append(out, map[string]any{
			"id": id, "title": title, "description": desc,
			"thesis": thesis, "card_count": cnt, "updated_at": updated,
		})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func getThinkingProjectAITool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing id")
	}
	var title, desc, thesis string
	var gap []byte
	err := d.QueryRowContext(ctx, `SELECT title, description, COALESCE(thesis,''), gap_questions FROM thinking_projects WHERE id=$1 AND user_id=$2`, id, uid).
		Scan(&title, &desc, &thesis, &gap)
	if err != nil {
		return nil, nil, fmt.Errorf("project not found")
	}
	var gapList []string
	json.Unmarshal(gap, &gapList)
	rows, err := d.QueryContext(ctx, `SELECT id, kind, content, status FROM thinking_cards WHERE project_id=$1 AND user_id=$2 ORDER BY created_at ASC`, id, uid)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	cards := []map[string]any{}
	for rows.Next() {
		var cid int64
		var kind, content, status string
		if err := rows.Scan(&cid, &kind, &content, &status); err != nil {
			return nil, nil, err
		}
		cards = append(cards, map[string]any{"id": cid, "kind": kind, "content": content, "status": status})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	events, err := thinking.LoadProjectEvents(ctx, d, uid, id)
	if err != nil {
		return nil, nil, err
	}
	for _, card := range cards {
		card["events"] = events[card["id"].(int64)]
	}
	return map[string]any{
		"id": id, "title": title, "description": desc,
		"thesis": thesis, "gap_questions": gapList,
		"cards": cards, "card_count": len(cards),
	}, nil, nil
}

func createThinkingProjectAITool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	title := argStr(args, "title")
	if title == "" {
		return nil, nil, fmt.Errorf("missing title")
	}
	desc := argStr(args, "description")
	var id int64
	err := d.QueryRowContext(ctx, `INSERT INTO thinking_projects (user_id, title, description) VALUES ($1,$2,$3) RETURNING id`, uid, title, desc).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"id": id, "title": title},
		map[string]any{"kind": "thinking_project_created", "id": id, "title": title, "route": fmt.Sprintf("/thinking/%d", id)}, nil
}

func addThoughtTool(ctx context.Context, s *Service, uid string, args map[string]any) (any, map[string]any, error) {
	d := s.db
	pid := argInt(args, "project_id", 0)
	content := argStr(args, "content")
	if pid == 0 || content == "" {
		return nil, nil, fmt.Errorf("missing project_id or content")
	}
	var ok int
	d.QueryRowContext(ctx, `SELECT 1 FROM thinking_projects WHERE id=$1 AND user_id=$2`, pid, uid).Scan(&ok)
	if ok != 1 {
		return nil, nil, fmt.Errorf("project not found")
	}
	kind := normalizeKindAI(argStr(args, "kind"))
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO thinking_cards (project_id, user_id, kind, content)
		VALUES ($1,$2,$3,$4) RETURNING id`, pid, uid, kind, content).Scan(&id)
	if err != nil {
		return nil, nil, err
	}
	d.ExecContext(ctx, `UPDATE thinking_projects SET updated_at=NOW(),context_updated_at=NOW() WHERE id=$1`, pid)

	// Async enrichment using sibling context (with prior enrichments).
	// userID is a UUID string; project + card ids stay int64 (BIGSERIAL).
	go func(cardID, projID int64, userID string) {
		s.EnrichCardWithContext(context.Background(), userID, projID, cardID)
	}(id, pid, uid)

	return map[string]any{"id": id, "kind": kind, "project_id": pid},
		map[string]any{"kind": "thinking_card_added", "id": id, "title": fmt.Sprintf("%s thought", kind), "route": fmt.Sprintf("/thinking/%d", pid)}, nil
}

func commentThinkingCardTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "card_id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing card_id")
	}
	if err := thinking.AddComment(ctx, d, uid, id, argStr(args, "comment")); err != nil {
		return nil, nil, err
	}
	var pid int64
	if err := d.QueryRowContext(ctx, `SELECT project_id FROM thinking_cards WHERE id=$1 AND user_id=$2`, id, uid).Scan(&pid); err != nil {
		return nil, nil, err
	}
	return map[string]any{"card_id": id, "commented": true}, map[string]any{"kind": "thinking_card_commented", "id": id, "route": fmt.Sprintf("/projects/%d", pid)}, nil
}

func setThinkingCardStateTool(ctx context.Context, d *db.DB, uid string, args map[string]any) (any, map[string]any, error) {
	id := argInt(args, "card_id", 0)
	if id == 0 {
		return nil, nil, fmt.Errorf("missing card_id")
	}
	closed := argBool(args, "closed", false)
	if err := thinking.SetClosed(ctx, d, uid, id, closed, argStr(args, "comment")); err != nil {
		return nil, nil, err
	}
	var pid int64
	if err := d.QueryRowContext(ctx, `SELECT project_id FROM thinking_cards WHERE id=$1 AND user_id=$2`, id, uid).Scan(&pid); err != nil {
		return nil, nil, err
	}
	return map[string]any{"card_id": id, "status": map[bool]string{true: "closed", false: "open"}[closed]}, map[string]any{"kind": "thinking_card_state_changed", "id": id, "route": fmt.Sprintf("/projects/%d", pid)}, nil
}

// enrichCardWithContext loads the target card + sibling cards (with
// their prior enrichments) and runs EnrichThinkingCard against it.
// Used by both the AI tool path (add_thought) and the HTTP path
// (api/thinking.go) — shared so we don't drift.
func (s *Service) EnrichCardWithContext(ctx context.Context, uid string, projectID, cardID int64) error {
	var target ThinkingCard
	if err := s.db.QueryRowContext(ctx, `SELECT id, kind, content, status FROM thinking_cards WHERE id=$1 AND user_id=$2`, cardID, uid).
		Scan(&target.ID, &target.Kind, &target.Content, &target.Status); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, content, ai_enrichment, status FROM thinking_cards WHERE project_id=$1 AND user_id=$2 AND id<>$3 ORDER BY created_at ASC`, projectID, uid, cardID)
	if err != nil {
		return err
	}
	defer rows.Close()
	siblings := []ThinkingCardWithEnrichment{}
	for rows.Next() {
		var c ThinkingCardWithEnrichment
		var er []byte
		if err := rows.Scan(&c.ID, &c.Kind, &c.Content, &er, &c.Status); err != nil {
			return err
		}
		if len(er) > 0 {
			var e ThinkingEnrichment
			if err := json.Unmarshal(er, &e); err == nil {
				c.Summary = e.Summary
				c.Connections = e.Connections
			}
		}
		siblings = append(siblings, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	events, err := thinking.LoadProjectEvents(ctx, s.db, uid, projectID)
	if err != nil {
		return err
	}
	target.Events = events[target.ID]
	for i := range siblings {
		siblings[i].Events = events[siblings[i].ID]
	}
	var pTitle, pDesc string
	s.db.QueryRowContext(ctx, `SELECT title, description FROM thinking_projects WHERE id=$1`, projectID).Scan(&pTitle, &pDesc)
	enrich, err := s.EnrichThinkingCard(ctx, pTitle, pDesc, target, siblings)
	if err != nil || enrich == nil {
		return err
	}
	ej, _ := json.Marshal(enrich)
	_, err = s.db.ExecContext(ctx, `UPDATE thinking_cards SET ai_enrichment=$1, enriched_at=NOW(), updated_at=NOW() WHERE id=$2 AND user_id=$3`, ej, cardID, uid)
	return err
}

// normalizeKindAI duplicates api.normalizeKind to avoid an import cycle.
func normalizeKindAI(k string) string {
	switch k {
	case "note", "entity", "question", "idea", "reflection", "claim", "fact",
		"hypothesis", "evidence", "contradiction", "decision", "todo":
		return k
	}
	return "note"
}
