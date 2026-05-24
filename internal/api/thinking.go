package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"sajni/internal/ai"
	"sajni/internal/db"
)

// Thinking mode: projects hold typed cards (note/entity/question/idea/
// reflection/claim/fact/hypothesis/evidence/contradiction/decision/todo).
// AI enriches each card using sibling-card context; synthesis writes
// thesis + gap-questions back onto the project.

var thinkingKinds = map[string]bool{
	"note": true, "entity": true, "question": true, "idea": true,
	"reflection": true, "claim": true, "fact": true, "hypothesis": true,
	"evidence": true, "contradiction": true, "decision": true, "todo": true,
}

func normalizeKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	if !thinkingKinds[k] {
		return "note"
	}
	return k
}

func registerThinkingRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/thinking/projects", listThinkingProjects(deps))
	mux.HandleFunc("POST /api/thinking/projects", createThinkingProject(deps))
	mux.HandleFunc("GET /api/thinking/projects/{id}", getThinkingProject(deps))
	mux.HandleFunc("PUT /api/thinking/projects/{id}", updateThinkingProject(deps))
	mux.HandleFunc("DELETE /api/thinking/projects/{id}", deleteThinkingProject(deps))
	mux.HandleFunc("POST /api/thinking/projects/{id}/synthesize", synthesizeThinkingProject(deps))

	mux.HandleFunc("POST /api/thinking/projects/{id}/cards", createThinkingCard(deps))
	mux.HandleFunc("PUT /api/thinking/cards/{id}", updateThinkingCard(deps))
	mux.HandleFunc("DELETE /api/thinking/cards/{id}", deleteThinkingCard(deps))
	mux.HandleFunc("POST /api/thinking/cards/{id}/enrich", enrichThinkingCard(deps))
	mux.HandleFunc("PUT /api/thinking/cards/{id}/enrichment", saveThinkingCardEnrichment(deps))

	mux.HandleFunc("POST /api/thinking/classify", classifyThinkingKind(deps))
}

// classifyThinkingKind powers the composer's auto-kind suggestion. The
// frontend debounces the user's typing, hits this with the current draft
// content, and silently flips the kind dropdown to the AI's guess.
func classifyThinkingKind(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AI == nil {
			errJSON(w, 503, "AI not configured")
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		opts := make([]string, 0, len(thinkingKinds))
		for k := range thinkingKinds {
			opts = append(opts, k)
		}
		sys := `You classify one personal thought-card into ONE kind:

- note: a fallback / untyped observation
- entity: a person, place, project, concept being named
- question: an open question the user is asking themselves
- idea: a creative proposal or "what if"
- reflection: a self-observation, feeling, retrospection
- claim: an assertion the user is making
- fact: a verifiable piece of information
- hypothesis: a candidate explanation worth testing
- evidence: data / quote / observation supporting another idea
- contradiction: a tension between two thoughts
- decision: a choice the user is making
- todo: a concrete action item

Pick the BEST single kind. Tie-break toward more specific (question > note, decision > todo when both fit).`
		kind, _, err := deps.AI.Classify(r.Context(), sys, body.Content, opts, "note")
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"kind": kind})
	}
}

func saveThinkingCardEnrichment(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		// Body is the full enrichment blob. Stored as-is so the user can
		// curate every field (summary/implications/questions/connections).
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			errJSON(w, 400, "invalid body")
			return
		}
		// Validate it's parseable JSON before persisting.
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		_, err = d.Exec(`UPDATE thinking_cards SET ai_enrichment=$1, enriched_at=NOW(), updated_at=NOW() WHERE id=$2 AND user_id=$3`, raw, id, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

type thinkingProjectRow struct {
	ID            int64           `json:"id"`
	Title         string          `json:"title"`
	Description   string          `json:"description"`
	Thesis        string          `json:"thesis"`
	GapQuestions  json.RawMessage `json:"gap_questions"`
	SynthesizedAt string          `json:"synthesized_at"`
	CardCount     int             `json:"card_count"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
}

type thinkingCardRow struct {
	ID           int64           `json:"id"`
	ProjectID    int64           `json:"project_id"`
	Kind         string          `json:"kind"`
	Content      string          `json:"content"`
	AIEnrichment json.RawMessage `json:"ai_enrichment"`
	EnrichedAt   string          `json:"enriched_at"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

func listThinkingProjects(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`
			SELECT p.id, p.title, p.description, p.thesis, p.gap_questions,
			       COALESCE(p.synthesized_at::text,''), COALESCE(c.cnt,0)::int,
			       p.created_at, p.updated_at
			FROM thinking_projects p
			LEFT JOIN (SELECT project_id, COUNT(*) cnt FROM thinking_cards GROUP BY project_id) c
			  ON c.project_id = p.id
			WHERE p.user_id=$1
			ORDER BY p.updated_at DESC`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []thinkingProjectRow{}
		for rows.Next() {
			var row thinkingProjectRow
			rows.Scan(&row.ID, &row.Title, &row.Description, &row.Thesis,
				&row.GapQuestions, &row.SynthesizedAt, &row.CardCount,
				&row.CreatedAt, &row.UpdatedAt)
			if len(row.GapQuestions) == 0 {
				row.GapQuestions = json.RawMessage("[]")
			}
			out = append(out, row)
		}
		writeJSON(w, 200, out)
	}
}

func createThinkingProject(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Title == "" {
			body.Title = "Untitled thinking"
		}
		var id int64
		err := d.QueryRow(
			`INSERT INTO thinking_projects (user_id, title, description) VALUES ($1,$2,$3) RETURNING id`,
			uid, body.Title, body.Description,
		).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func getThinkingProject(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var p thinkingProjectRow
		err = d.QueryRow(`
			SELECT id, title, description, thesis, gap_questions,
			       COALESCE(synthesized_at::text,''), 0, created_at, updated_at
			FROM thinking_projects WHERE id=$1 AND user_id=$2`, id, uid).
			Scan(&p.ID, &p.Title, &p.Description, &p.Thesis, &p.GapQuestions,
				&p.SynthesizedAt, &p.CardCount, &p.CreatedAt, &p.UpdatedAt)
		if err != nil {
			errJSON(w, 404, "not found")
			return
		}
		if len(p.GapQuestions) == 0 {
			p.GapQuestions = json.RawMessage("[]")
		}
		cards, err := loadProjectCardRows(d, uid, id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		p.CardCount = len(cards)
		writeJSON(w, 200, map[string]any{"project": p, "cards": cards})
	}
}

func updateThinkingProject(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Title       *string `json:"title"`
			Description *string `json:"description"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Title != nil {
			d.Exec(`UPDATE thinking_projects SET title=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3`, *body.Title, id, uid)
		}
		if body.Description != nil {
			d.Exec(`UPDATE thinking_projects SET description=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3`, *body.Description, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteThinkingProject(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec(`DELETE FROM thinking_projects WHERE id=$1 AND user_id=$2`, id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func createThinkingCard(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		pid, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid project id")
			return
		}
		var ok int
		d.QueryRow(`SELECT 1 FROM thinking_projects WHERE id=$1 AND user_id=$2`, pid, uid).Scan(&ok)
		if ok != 1 {
			errJSON(w, 404, "project not found")
			return
		}
		var body struct {
			Kind    string `json:"kind"`
			Content string `json:"content"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		var id int64
		err = d.QueryRow(`
			INSERT INTO thinking_cards (project_id, user_id, kind, content)
			VALUES ($1,$2,$3,$4) RETURNING id`,
			pid, uid, normalizeKind(body.Kind), body.Content).Scan(&id)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		d.Exec(`UPDATE thinking_projects SET updated_at=NOW() WHERE id=$1`, pid)

		if deps.AI != nil {
			go func(cardID, projID, userID int64) {
				bg := context.Background()
				if err := runEnrichment(bg, deps, userID, projID, cardID); err == nil {
					// Re-enrich neighbors so they can pick up the new
					// card as a connection target.
					reEnrichNeighbors(deps, userID, projID, cardID)
				}
			}(id, pid, uid)
		}

		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateThinkingCard(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Kind    *string `json:"kind"`
			Content *string `json:"content"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Kind != nil {
			d.Exec(`UPDATE thinking_cards SET kind=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3`, normalizeKind(*body.Kind), id, uid)
		}
		if body.Content != nil {
			d.Exec(`UPDATE thinking_cards SET content=$1, updated_at=NOW() WHERE id=$2 AND user_id=$3`, *body.Content, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteThinkingCard(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec(`DELETE FROM thinking_cards WHERE id=$1 AND user_id=$2`, id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func enrichThinkingCard(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		if deps.AI == nil {
			errJSON(w, 503, "AI not configured")
			return
		}
		var pid int64
		err = d.QueryRow(`SELECT project_id FROM thinking_cards WHERE id=$1 AND user_id=$2`, id, uid).Scan(&pid)
		if err != nil {
			errJSON(w, 404, "card not found")
			return
		}
		if err := runEnrichment(r.Context(), deps, uid, pid, id); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func synthesizeThinkingProject(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		if deps.AI == nil {
			errJSON(w, 503, "AI not configured")
			return
		}
		var title, desc string
		err = d.QueryRow(`SELECT title, description FROM thinking_projects WHERE id=$1 AND user_id=$2`, id, uid).Scan(&title, &desc)
		if err != nil {
			errJSON(w, 404, "project not found")
			return
		}
		cards, err := loadProjectCardsWithEnrichment(d, uid, id, 0)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		thesis, questions, err := deps.AI.SynthesizeThinking(r.Context(), title, desc, cards)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		qj, _ := json.Marshal(questions)
		d.Exec(`UPDATE thinking_projects SET thesis=$1, gap_questions=$2, synthesized_at=NOW(), updated_at=NOW() WHERE id=$3 AND user_id=$4`,
			thesis, qj, id, uid)
		writeJSON(w, 200, map[string]any{"thesis": thesis, "gap_questions": questions})
	}
}

// runEnrichment is a thin shim around the AI service helper so both
// the HTTP path and the agent-tool path go through the same logic.
func runEnrichment(ctx context.Context, deps Deps, uid, projectID, cardID int64) error {
	return deps.AI.EnrichCardWithContext(ctx, uid, projectID, cardID)
}

// reEnrichNeighbors picks the 3 most-recent OTHER cards in the project
// and re-runs enrichment on each so they can discover the freshly
// added card as a connection target. Runs in the background; failure
// per-card is logged-and-swallowed.
func reEnrichNeighbors(deps Deps, uid, projectID, justAddedCardID int64) {
	d := deps.DB
	rows, err := d.Query(`
		SELECT id FROM thinking_cards
		WHERE project_id=$1 AND user_id=$2 AND id<>$3
		ORDER BY created_at DESC LIMIT 3`, projectID, uid, justAddedCardID)
	if err != nil {
		return
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	for _, id := range ids {
		go func(cid int64) {
			runEnrichment(context.Background(), deps, uid, projectID, cid)
		}(id)
	}
}

func loadProjectCardRows(d *db.DB, uid, pid int64) ([]thinkingCardRow, error) {
	rows, err := d.Query(`
		SELECT id, project_id, kind, content, ai_enrichment,
		       COALESCE(enriched_at::text,''), created_at, updated_at
		FROM thinking_cards WHERE project_id=$1 AND user_id=$2
		ORDER BY created_at ASC`, pid, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []thinkingCardRow{}
	for rows.Next() {
		var c thinkingCardRow
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Kind, &c.Content, &c.AIEnrichment,
			&c.EnrichedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		if len(c.AIEnrichment) == 0 {
			c.AIEnrichment = json.RawMessage("{}")
		}
		out = append(out, c)
	}
	return out, nil
}

// loadProjectCardsWithEnrichment loads siblings WITH their prior
// enrichment fields (summary + outbound connections) so the AI can
// build on the existing graph instead of reinterpreting from raw text
// every time. Pass excludeID > 0 to drop a specific card (the target).
func loadProjectCardsWithEnrichment(d *db.DB, uid, pid, excludeID int64) ([]ai.ThinkingCardWithEnrichment, error) {
	rows, err := d.Query(`
		SELECT id, kind, content, ai_enrichment
		FROM thinking_cards
		WHERE project_id=$1 AND user_id=$2 AND ($3 = 0 OR id <> $3)
		ORDER BY created_at ASC`, pid, uid, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ai.ThinkingCardWithEnrichment{}
	for rows.Next() {
		var c ai.ThinkingCardWithEnrichment
		var enrichRaw []byte
		if err := rows.Scan(&c.ID, &c.Kind, &c.Content, &enrichRaw); err != nil {
			return nil, err
		}
		if len(enrichRaw) > 0 {
			var e ai.ThinkingEnrichment
			if err := json.Unmarshal(enrichRaw, &e); err == nil {
				c.Summary = e.Summary
				c.Connections = e.Connections
			}
		}
		out = append(out, c)
	}
	return out, nil
}
