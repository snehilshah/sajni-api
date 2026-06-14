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

	"github.com/rs/zerolog/log"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/push"
	"sajni/internal/storage"
)

// Deps bundles the runtime dependencies API handlers need.
type Deps struct {
	DB      *db.DB
	Auth    *auth.Service
	Storage storage.Storage
	AI      *ai.Service  // nil when GEMINI_API_KEY is unset
	Push    *push.Sender // nil when FIREBASE_PROJECT_ID is unset
	// AILimiter is shared across all AI endpoints (chat, palette,
	// categorize, …) so a single per-user budget governs total spend.
	AILimiter *aiLimiter
}

// Router builds the top-level HTTP handler. Auth routes are mounted
// outside of the protected zone; everything else requires a valid
// access token via auth.Middleware.
func Router(deps Deps, frontendDir string) http.Handler {
	if deps.AILimiter == nil {
		deps.AILimiter = newAILimiter()
	}
	authMux := http.NewServeMux()
	deps.Auth.RegisterRoutes(authMux)

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /api/auth/me", deps.Auth.HandleMe)
	apiMux.HandleFunc("POST /api/auth/profile", deps.Auth.HandleUpdateProfile)
	apiMux.HandleFunc("POST /api/auth/onboarded", deps.Auth.HandleOnboarded)
	apiMux.HandleFunc("POST /api/auth/timezone", deps.Auth.HandleSetTimezone)
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
	registerBillerRoutes(apiMux, deps)
	registerInsightRoutes(apiMux, deps)
	registerThemeRoutes(apiMux, deps)
	registerSearchRoutes(apiMux, deps)
	registerAIRoutes(apiMux, deps, deps.AI)
	registerThinkingRoutes(apiMux, deps)
	registerTakeoutRoutes(apiMux, deps)
	registerLinkRoutes(apiMux, deps)
	registerBookmarkRoutes(apiMux, deps)
	registerPushRoutes(apiMux, deps)

	protected := deps.Auth.Middleware(apiMux)

	// Top-level dispatcher: route the unauthenticated auth endpoints
	// directly to authMux, fall through to the protected mux for
	// everything else (including /api/auth/me + /api/auth/onboarded).
	root := http.NewServeMux()
	root.Handle("/api/auth/google/start", authMux)
	root.Handle("/api/auth/google/callback", authMux)
	root.Handle("/api/auth/github/start", authMux)
	root.Handle("/api/auth/github/callback", authMux)
	root.Handle("/api/auth/email/start", authMux)
	root.Handle("/api/auth/email/verify", authMux)
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

	// Internal webhooks. Auth is by a shared secret header; each 401s without
	// it. Insights and reminder sweeps run by Cloud Scheduler; exact reminder
	// fires run by Cloud Tasks.
	RegisterInsightCronHandler(root, deps)
	RegisterReminderCronHandler(root, deps)
	RegisterPriceCronHandler(root, deps)

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

	return withCORS(withLogging(root))
}

// withLogging logs all requests at debug, errors (5xx) at error, slow (>2s) at warn.
// Health probes are skipped entirely.
func withLogging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			h.ServeHTTP(w, r)
			return
		}
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		h.ServeHTTP(rw, r)
		dur := time.Since(start)

		evt := log.Debug()
		switch {
		case rw.status >= 500:
			evt = log.Error()
		case dur > 2*time.Second:
			evt = log.Warn()
		}
		evt.Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rw.status).
			Int64("ms", dur.Milliseconds()).
			Send()
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying ResponseWriter so SSE and chunked
// transfers work through the logging middleware.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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

// userID returns the authenticated UUID (string) for the request, or
// panics if auth middleware did not run. Centralizes the type so every
// handler keeps using `userID(ctx)` regardless of the underlying type.
func userID(ctx context.Context) string {
	return auth.MustUserID(ctx)
}
