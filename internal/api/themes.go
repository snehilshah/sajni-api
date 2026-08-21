package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"sajni/internal/theme"
)

func registerThemeRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/themes", listThemesHandler(deps))
	mux.HandleFunc("GET /api/themes/active", getActiveTheme(deps))
	mux.HandleFunc("POST /api/themes/generate", generateThemeHandler(deps))
	mux.HandleFunc("DELETE /api/themes/{id}", deleteThemeHandler(deps))
	mux.HandleFunc("POST /api/themes/deactivate", deactivateThemeHandler(deps))
	mux.HandleFunc("POST /api/themes/{id}/activate", activateThemeHandler(deps))
}

func listThemesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		rows, err := deps.DB.QueryContext(r.Context(), `SELECT id, name, seeds, prompt, is_active, created_at::text
			FROM user_themes WHERE user_id = $1
			ORDER BY created_at DESC, id DESC`, uid)
		if err != nil {
			internalError(w, r, "list themes", err)
			return
		}
		defer rows.Close()

		out := []theme.Theme{}
		for rows.Next() {
			var t theme.Theme
			var seedsRaw []byte
			if err := rows.Scan(&t.ID, &t.Name, &seedsRaw, &t.Prompt, &t.IsActive, &t.CreatedAt); err != nil {
				internalError(w, r, "scan theme", err)
				return
			}
			if err := json.Unmarshal(seedsRaw, &t.Seeds); err != nil {
				internalError(w, r, "decode theme seeds", err)
				return
			}
			if err := theme.ValidateSeeds(&t.Seeds); err != nil {
				internalError(w, r, "validate theme seeds", err)
				return
			}
			out = append(out, t)
		}
		if err := rows.Err(); err != nil {
			internalError(w, r, "iterate themes", err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func getActiveTheme(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var t theme.Theme
		var seedsRaw []byte
		err := deps.DB.QueryRowContext(r.Context(), `SELECT id, name, seeds, prompt, is_active, created_at::text
			FROM user_themes WHERE user_id = $1 AND is_active = TRUE LIMIT 1`, uid).
			Scan(&t.ID, &t.Name, &seedsRaw, &t.Prompt, &t.IsActive, &t.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusOK, nil)
			return
		}
		if err != nil {
			internalError(w, r, "get active theme", err)
			return
		}
		if err := json.Unmarshal(seedsRaw, &t.Seeds); err != nil {
			internalError(w, r, "decode active theme seeds", err)
			return
		}
		if err := theme.ValidateSeeds(&t.Seeds); err != nil {
			internalError(w, r, "validate active theme seeds", err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}
}

func generateThemeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.AI == nil {
			errJSON(w, http.StatusServiceUnavailable, "AI not configured")
			return
		}
		var body struct {
			Prompt string `json:"prompt"`
		}
		if err := readJSON(r, &body); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid json")
			return
		}
		if strings.TrimSpace(body.Prompt) == "" {
			errJSON(w, http.StatusBadRequest, "missing prompt")
			return
		}
		t, err := theme.Generate(r.Context(), deps.AI, deps.DB, userID(r.Context()), body.Prompt)
		if err != nil {
			internalError(w, r, "generate theme", err)
			return
		}
		writeJSON(w, http.StatusCreated, t)
	}
}

func activateThemeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		id, err := intParam(r, "id")
		if err != nil || id <= 0 {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		if err := theme.Activate(r.Context(), deps.DB, uid, id); err != nil {
			if errors.Is(err, theme.ErrNotFound) {
				errJSON(w, http.StatusNotFound, err.Error())
				return
			}
			internalError(w, r, "activate theme", err)
			return
		}
		t, err := theme.Load(r.Context(), deps.DB, uid, id)
		if err != nil {
			internalError(w, r, "load activated theme", err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}
}

func deactivateThemeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := theme.Deactivate(r.Context(), deps.DB, userID(r.Context())); err != nil {
			internalError(w, r, "deactivate theme", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func deleteThemeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := intParam(r, "id")
		if err != nil || id <= 0 {
			errJSON(w, http.StatusBadRequest, "invalid id")
			return
		}
		result, err := deps.DB.ExecContext(r.Context(),
			`DELETE FROM user_themes WHERE id = $1 AND user_id = $2`, id, userID(r.Context()))
		if err != nil {
			internalError(w, r, "delete theme", err)
			return
		}
		if affected, err := result.RowsAffected(); err != nil {
			internalError(w, r, "count deleted themes", err)
			return
		} else if affected == 0 {
			errJSON(w, http.StatusNotFound, theme.ErrNotFound.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
