package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/storage"
)

// Deps bundles the runtime dependencies API handlers need.
type Deps struct {
	DB      *db.DB
	Auth    *auth.Service
	Storage storage.Storage
	AI      *ai.Service // nil when GEMINI_API_KEY is unset
}

// Router builds the top-level HTTP handler. Auth routes are mounted
// outside of the protected zone; everything else requires a valid
// access token via auth.Middleware.
func Router(deps Deps, frontendDir string) http.Handler {
	authMux := http.NewServeMux()
	deps.Auth.RegisterRoutes(authMux)

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /api/auth/me", deps.Auth.HandleMe)
	registerMemoRoutes(apiMux, deps)
	registerTaskRoutes(apiMux, deps)
	registerTaskListRoutes(apiMux, deps)
	registerHabitRoutes(apiMux, deps)
	registerMediaRoutes(apiMux, deps)
	registerJournalRoutes(apiMux, deps)
	registerNoteRoutes(apiMux, deps)
	registerUploadRoutes(apiMux, deps)
	registerTagRoutes(apiMux, deps)
	registerAnalyticsRoutes(apiMux, deps)
	registerFinanceRoutes(apiMux, deps)
	registerSearchRoutes(apiMux, deps)
	registerAIRoutes(apiMux, deps, deps.AI)

	protected := deps.Auth.Middleware(apiMux)

	// Top-level dispatcher: route auth endpoints first (no token), then
	// fall through to the protected mux for everything else.
	root := http.NewServeMux()
	root.Handle("/api/auth/register", authMux)
	root.Handle("/api/auth/login", authMux)
	root.Handle("/api/auth/refresh", authMux)
	root.Handle("/api/auth/logout", authMux)
	root.Handle("/api/", protected)

	// Liveness/readiness probes — unauthenticated, ping the DB so deploys
	// can fail fast if Postgres is unreachable.
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := deps.DB.PingContext(ctx); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}
	root.HandleFunc("/healthz", healthHandler)
	root.HandleFunc("/readyz", healthHandler)

	if frontendDir != "" {
		fs := http.FileServer(http.Dir(frontendDir))
		root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			path := filepath.Join(frontendDir, r.URL.Path)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				fs.ServeHTTP(w, r)
				return
			}
			http.ServeFile(w, r, filepath.Join(frontendDir, "index.html"))
		})
	}

	return withCORS(root)
}

// withCORS reflects the configured allowed origin and enables credentials
// so the refresh-token cookie can travel across origins.
func withCORS(h http.Handler) http.Handler {
	allowed := os.Getenv("CORS_ORIGIN")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowed != "" && origin == allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else if allowed == "" && origin != "" {
			// Dev fallback: reflect any origin when no allow-list is configured.
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func pathParam(r *http.Request, name string) string {
	return r.PathValue(name)
}

func queryParam(r *http.Request, name string) string {
	return r.URL.Query().Get(name)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func intParam(r *http.Request, name string) (int64, error) {
	s := pathParam(r, name)
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func userID(ctx context.Context) int64 {
	return auth.MustUserID(ctx)
}
