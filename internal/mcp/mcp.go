// Package mcp lets outside assistants (ChatGPT, Gemini, Claude) use Sajni's
// AI tools over the Model Context Protocol. It has two halves:
//
//   - an OAuth 2.1 authorization server (oauth.go): discovery documents,
//     client registration (DCR and client metadata documents), the consent
//     page's API, and the token endpoint. The consent page itself is the
//     web route /oauth/authorize.
//   - the MCP endpoint, POST /mcp (rpc.go): stateless streamable HTTP with
//     plain JSON replies, serving the same tool registry as the in-app chat.
//
// The public URLs sit on the web origin (APP_URL) and reach this API
// through Vercel rewrites, so the server URL to paste is APP_URL + "/mcp".
package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"sajni/internal/ai"
	"sajni/internal/db"
)

// Scopes. A connection always reads; "write" adds the tools that change things.
const (
	scopeRead  = "read"
	scopeWrite = "write"
)

// Server serves the OAuth and MCP endpoints.
type Server struct {
	db   *db.DB
	ai   *ai.Service // nil without GEMINI_API_KEY: no tools
	base string      // public origin, e.g. https://www.ohmysajni.com

	fetch *http.Client // reads client metadata documents
	reg   *window      // caps open registrations

	toolsOnce sync.Once
	tools     []toolEntry
}

// New builds the server. appURL is the public web origin.
func New(d *db.DB, svc *ai.Service, appURL string) *Server {
	return &Server{
		db:    d,
		ai:    svc,
		base:  strings.TrimRight(appURL, "/"),
		fetch: metadataClient(),
		reg:   newWindow(60),
	}
}

func (s *Server) resourceURL() string { return s.base + "/mcp" }
func (s *Server) prmURL() string      { return s.base + "/.well-known/oauth-protected-resource/mcp" }

// RegisterPublic mounts the endpoints that take no Sajni session.
func (s *Server) RegisterPublic(mux *http.ServeMux) {
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authServer)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.authServer)
	mux.HandleFunc("POST /api/oauth/register", s.register)
	mux.HandleFunc("POST /api/oauth/token", s.token)
	mux.HandleFunc("POST /api/oauth/revoke", s.revoke)
}

// RegisterUser mounts the endpoints the signed-in web app calls: the
// consent page and Settings → Connected apps.
func (s *Server) RegisterUser(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/oauth/request", s.consentInfo)
	mux.HandleFunc("POST /api/oauth/approve", s.approve)
	mux.HandleFunc("GET /api/connected-apps", s.listApps)
	mux.HandleFunc("DELETE /api/connected-apps/{id}", s.removeApp)
}

// IsPublicPath reports paths any origin may call from a browser (no
// cookies involved), for the CORS layer.
func IsPublicPath(p string) bool {
	return p == "/mcp" || strings.HasPrefix(p, "/.well-known/") ||
		p == "/api/oauth/register" || p == "/api/oauth/token" || p == "/api/oauth/revoke"
}

func (s *Server) protectedResource(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.resourceURL(),
		"resource_name":            "Sajni",
		"authorization_servers":    []string{s.base},
		"scopes_supported":         []string{scopeRead, scopeWrite},
		"bearer_methods_supported": []string{"header"},
		"resource_documentation":   s.base + "/docs/connected-apps",
	})
}

func (s *Server) authServer(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.base,
		"authorization_endpoint":                         s.base + "/oauth/authorize",
		"token_endpoint":                                 s.base + "/api/oauth/token",
		"registration_endpoint":                          s.base + "/api/oauth/register",
		"revocation_endpoint":                            s.base + "/api/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_post", "client_secret_basic"},
		"revocation_endpoint_auth_methods_supported":     []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                               []string{scopeRead, scopeWrite},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
		"service_documentation":                          s.base + "/docs/connected-apps",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
