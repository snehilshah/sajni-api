package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/config"
	"sajni/internal/db"
	"sajni/internal/mcp"
	"sajni/internal/push"
	"sajni/internal/reminderqueue"
	"sajni/internal/storage"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// Deps bundles the runtime dependencies API handlers need.
type Deps struct {
	Environment   config.Environment
	HTTP          config.HTTP
	Media         config.Media
	Reminders     config.Reminders
	ReminderQueue reminderqueue.Queue
	DB            *db.DB
	Auth          *auth.Service
	Storage       storage.Storage
	AI            *ai.Service  // nil when GEMINI_API_KEY is unset
	Push          *push.Sender // nil when FIREBASE_PROJECT_ID is unset
	// AILimiter is shared across all AI endpoints (chat, palette,
	// categorize, …) so a single per-user budget governs total spend.
	AILimiter *aiLimiter
}

// Router builds the top-level HTTP handler. Auth routes are mounted
// outside of the protected zone; everything else requires a valid
// access token via auth.Middleware.
func Router(deps Deps) http.Handler {
	if deps.AILimiter == nil {
		deps.AILimiter = newAILimiter()
	}
	authMux := http.NewServeMux()
	deps.Auth.RegisterRoutes(authMux)

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /api/auth/me", deps.Auth.HandleMe)
	apiMux.HandleFunc("POST /api/auth/profile", deps.Auth.HandleUpdateProfile)
	apiMux.HandleFunc("POST /api/auth/profile/avatar/reroll", deps.Auth.HandleRerollAvatar)
	apiMux.HandleFunc("POST /api/auth/timezone", deps.Auth.HandleSetTimezone)
	apiMux.HandleFunc("POST /api/auth/notify-channel", deps.Auth.HandleSetNotifyChannel)
	registerMemoRoutes(apiMux, deps)
	registerTaskRoutes(apiMux, deps)
	registerPlannerRoutes(apiMux, deps)
	registerReminderRoutes(apiMux, deps)
	registerPlanRoutes(apiMux, deps)
	registerTaskListRoutes(apiMux, deps)
	registerHabitRoutes(apiMux, deps)
	registerEventRoutes(apiMux, deps)
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
	// Connected apps: outside assistants over MCP (internal/mcp).
	connected := mcp.New(deps.DB, deps.AI, deps.Auth.AppURL)
	connected.RegisterUser(apiMux)

	protected := deps.Auth.Middleware(withIdempotency(deps.DB, apiMux))

	// Top-level dispatcher: route the unauthenticated auth endpoints
	// directly to authMux, fall through to the protected mux for
	// everything else (including /api/auth/me).
	root := http.NewServeMux()
	root.Handle("/api/auth/google/start", authMux)
	root.Handle("/api/auth/google/callback", authMux)
	root.Handle("/api/auth/github/start", authMux)
	root.Handle("/api/auth/github/callback", authMux)
	root.Handle("/api/auth/email/start", authMux)
	root.Handle("/api/auth/email/verify", authMux)
	root.Handle("/api/auth/mobile/exchange", authMux)
	root.Handle("/api/auth/refresh", authMux)
	root.Handle("/api/auth/logout", authMux)
	root.Handle("/api/", protected)
	// Client diagnostics: open so a broken session can still report.
	root.Handle("POST /api/telemetry", handleTelemetry(deps))
	connected.RegisterPublic(root)

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
	// it. Reminder and notification sweeps run by Cloud Scheduler; exact
	// reminder fires run by Cloud Tasks.
	RegisterReminderCronHandler(root, deps)
	RegisterDigestCronHandler(root, deps)
	RegisterScheduledNotificationHandler(root, deps)

	return withCORS(withLogging(root, deps.DB, deps.Reminders.CloudTasksProject), deps.HTTP)
}

// withCORS reflects the configured allowed origin and enables credentials
// so the refresh-token cookie can travel across origins.
func withCORS(h http.Handler, cfg config.HTTP) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if mcp.IsPublicPath(r.URL.Path) {
			// MCP and OAuth endpoints use bearer tokens, never cookies, so
			// any origin may call them (browser-based MCP clients).
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Mcp-Protocol-Version, Mcp-Session-Id, Last-Event-ID")
			w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.ServeHTTP(w, r)
			return
		}
		if cfg.CORSOrigin != "" && origin == cfg.CORSOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else if cfg.AllowLocalCORS && corsLocalPrivateOrigin(origin) {
			// Explicit local-only mode for Vite or a phone on the development LAN.
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func corsLocalPrivateOrigin(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback() || addr.IsPrivate()
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
	if status >= http.StatusInternalServerError {
		msg = "internal error"
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

func internalError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	log.Error().Err(err).
		Str("operation", operation).
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Msg("request failed")
	errJSON(w, http.StatusInternalServerError, "internal error")
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
