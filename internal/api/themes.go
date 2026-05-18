package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"sajni/internal/theme"
)

// Themes = user-owned M3 color palettes. Each row stores 2–4 seed hex
// colors; the frontend derives the full token set via Google's
// material-color-utilities. The heavy lifting (Gemini prompt, hex
// validation, atomic activation) lives in internal/theme so both the
// HTTP handler below and the AI tool can share it.

func registerThemeRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/themes", listThemesHandler(deps))
	mux.HandleFunc("GET /api/themes/active", getActiveTheme(deps))
	mux.HandleFunc("POST /api/themes", createThemeHandler(deps))
	mux.HandleFunc("POST /api/themes/generate", generateThemeHandler(deps))
	mux.HandleFunc("PUT /api/themes/{id}", updateThemeHandler(deps))
	mux.HandleFunc("DELETE /api/themes/{id}", deleteThemeHandler(deps))
	mux.HandleFunc("POST /api/themes/{id}/activate", activateThemeHandler(deps))
}

func listThemesHandler(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := d.Query(`SELECT id, name, source, seeds, prompt, mode_pref, is_active, created_at::text
			FROM user_themes WHERE user_id = $1
			ORDER BY is_active DESC, created_at DESC`, uid)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []theme.Theme{}
		for rows.Next() {
			var t theme.Theme
			var seedsRaw []byte
			rows.Scan(&t.ID, &t.Name, &t.Source, &seedsRaw, &t.Prompt, &t.ModePref, &t.IsActive, &t.CreatedAt)
			json.Unmarshal(seedsRaw, &t.Seeds)
			out = append(out, t)
		}
		writeJSON(w, 200, out)
	}
}

func getActiveTheme(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var t theme.Theme
		var seedsRaw []byte
		err := d.QueryRow(`SELECT id, name, source, seeds, prompt, mode_pref, is_active, created_at::text
			FROM user_themes WHERE user_id = $1 AND is_active = TRUE LIMIT 1`, uid).
			Scan(&t.ID, &t.Name, &t.Source, &seedsRaw, &t.Prompt, &t.ModePref, &t.IsActive, &t.CreatedAt)
		if err != nil {
			writeJSON(w, 200, nil)
			return
		}
		json.Unmarshal(seedsRaw, &t.Seeds)
		writeJSON(w, 200, t)
	}
}

func createThemeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Name     string      `json:"name"`
			Seeds    theme.Seeds `json:"seeds"`
			Source   string      `json:"source"`
			Prompt   string      `json:"prompt"`
			ModePref string      `json:"mode_pref"`
			Activate bool        `json:"activate"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if err := theme.ValidateSeeds(&body.Seeds); err != nil {
			errJSON(w, 400, err.Error())
			return
		}
		if body.Source == "" {
			body.Source = "manual"
		}
		if body.ModePref == "" {
			body.ModePref = "auto"
		}
		if body.Name == "" {
			body.Name = "Untitled theme"
		}
		id, err := theme.Insert(r.Context(), deps.DB, uid, body.Name, body.Source, body.Prompt, body.ModePref, body.Seeds, body.Activate)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, map[string]int64{"id": id})
	}
}

func updateThemeHandler(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		var body struct {
			Name     *string      `json:"name"`
			Seeds    *theme.Seeds `json:"seeds"`
			ModePref *string      `json:"mode_pref"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if body.Name != nil {
			d.Exec(`UPDATE user_themes SET name = $1 WHERE id = $2 AND user_id = $3`, *body.Name, id, uid)
		}
		if body.Seeds != nil {
			if err := theme.ValidateSeeds(body.Seeds); err != nil {
				errJSON(w, 400, err.Error())
				return
			}
			seedsRaw, _ := json.Marshal(body.Seeds)
			d.Exec(`UPDATE user_themes SET seeds = $1 WHERE id = $2 AND user_id = $3`, seedsRaw, id, uid)
		}
		if body.ModePref != nil {
			d.Exec(`UPDATE user_themes SET mode_pref = $1 WHERE id = $2 AND user_id = $3`, *body.ModePref, id, uid)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func deleteThemeHandler(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		d.Exec("DELETE FROM user_themes WHERE id = $1 AND user_id = $2", id, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func activateThemeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil {
			errJSON(w, 400, "invalid id")
			return
		}
		if err := theme.Activate(r.Context(), deps.DB, uid, id); err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		t, lerr := theme.Load(r.Context(), deps.DB, uid, id)
		if lerr != nil {
			writeJSON(w, 200, map[string]string{"status": "ok"})
			return
		}
		writeJSON(w, 200, t)
	}
}

func generateThemeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AI == nil {
			errJSON(w, 503, "AI not configured")
			return
		}
		uid := userID(r.Context())
		var body struct {
			Prompt   string `json:"prompt"`
			Activate bool   `json:"activate"`
			ModePref string `json:"mode_pref"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, 400, "invalid json")
			return
		}
		if strings.TrimSpace(body.Prompt) == "" {
			errJSON(w, 400, "missing prompt")
			return
		}
		t, err := theme.Generate(r.Context(), deps.AI, deps.DB, uid, body.Prompt, body.ModePref, body.Activate)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, t)
	}
}
