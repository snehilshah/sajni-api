package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/genai"

	"sajni/internal/ai"
)

// Protocol versions this server speaks, newest first. All the methods used
// here are the same across them.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// caller is who an access token belongs to.
type caller struct {
	uid    string
	grant  string
	client string // app name, for logs
	write  bool
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// handleMCP is the streamable HTTP endpoint. Stateless: every POST carries
// its own bearer token and gets a plain JSON reply; there is no SSE stream.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	who, err := s.authenticate(r)
	if err != nil {
		s.challenge(w, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	var msgs []rpcMessage
	batch := len(body) > 0 && body[0] == '['
	if batch {
		err = json.Unmarshal(body, &msgs)
	} else {
		var m rpcMessage
		err = json.Unmarshal(body, &m)
		msgs = []rpcMessage{m}
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	var out []rpcResponse
	for _, m := range msgs {
		// Notifications and replies from the client need no answer.
		if len(m.ID) == 0 || string(m.ID) == "null" || m.Method == "" {
			continue
		}
		res, rerr := s.dispatch(r.Context(), who, m)
		out = append(out, rpcResponse{JSONRPC: "2.0", ID: m.ID, Result: res, Error: rerr})
	}
	switch {
	case len(out) == 0:
		w.WriteHeader(http.StatusAccepted)
	case batch:
		writeJSON(w, http.StatusOK, out)
	default:
		writeJSON(w, http.StatusOK, out[0])
	}
}

func (s *Server) dispatch(ctx context.Context, who *caller, m rpcMessage) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		version := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "sajni", "title": "Sajni", "version": "1.0.0", "websiteUrl": s.base},
			"instructions":    ai.MCPInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := []map[string]any{}
		for _, t := range s.toolList() {
			if who.write || !t.tool.Mutating {
				list = append(list, t.descriptor)
			}
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		return s.callTool(ctx, who, p.Name, p.Arguments)
	}
	return nil, &rpcError{-32601, "method not found: " + m.Method}
}

func (s *Server) callTool(ctx context.Context, who *caller, name string, args map[string]any) (any, *rpcError) {
	var entry *toolEntry
	for i, t := range s.toolList() {
		if t.tool.Name == name {
			entry = &s.toolList()[i]
			break
		}
	}
	if entry == nil {
		return nil, &rpcError{-32602, "unknown tool: " + name}
	}
	if entry.tool.Mutating && !who.write {
		return toolText("This connection can only read Sajni. To allow changes, remove Sajni from this app and connect it again with changes allowed.", true), nil
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	data, err := s.ai.CallTool(ctx, who.uid, name, args)
	ev := log.Info()
	var res map[string]any
	if err != nil {
		ev = log.Warn().Err(err)
		res = toolText("Error: "+err.Error(), true)
	} else {
		b, merr := json.Marshal(data)
		if merr != nil {
			b = []byte(`{"error":"result could not be encoded"}`)
		}
		ev = ev.Int("result_bytes", len(b))
		res = toolText(string(b), false)
	}
	ev.Str("uid", who.uid).Str("client", who.client).Str("tool", name).Bool("ok", err == nil).
		Int64("ms", time.Since(start).Milliseconds()).Msg("mcp call")
	return res, nil
}

func toolText(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// ---------- auth ----------

var errNoToken = errors.New("no token")

// authenticate resolves the bearer token to its user and grant.
func (s *Server) authenticate(r *http.Request) (*caller, error) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		tok, ok = strings.CutPrefix(h, "bearer ")
	}
	tok = strings.TrimSpace(tok)
	if !ok || tok == "" {
		return nil, errNoToken
	}
	var who caller
	var scope string
	var stale bool
	err := s.db.QueryRowContext(r.Context(), `
		SELECT g.id, g.user_id, g.scope, c.name,
		       g.last_used_at IS NULL OR g.last_used_at < NOW() - INTERVAL '5 minutes'
		FROM mcp_tokens t
		JOIN mcp_grants g ON g.id = t.grant_id
		JOIN mcp_clients c ON c.id = g.client_id
		JOIN users u ON u.id = g.user_id AND u.deleted_at IS NULL
		WHERE t.token_hash = $1 AND t.kind = 'access' AND t.expires_at > NOW()`, hash(tok)).
		Scan(&who.grant, &who.uid, &scope, &who.client, &stale)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Warn().Err(err).Msg("mcp token lookup")
		}
		return nil, errors.New("invalid token")
	}
	who.write = strings.Contains(scope, scopeWrite)
	if stale {
		s.db.ExecContext(r.Context(), `UPDATE mcp_grants SET last_used_at = NOW() WHERE id = $1`, who.grant)
	}
	return &who, nil
}

// challenge sends the 401 that starts an app's sign-in (RFC 9728).
func (s *Server) challenge(w http.ResponseWriter, err error) {
	v := `Bearer resource_metadata="` + s.prmURL() + `", scope="` + scopeRead + " " + scopeWrite + `"`
	if !errors.Is(err, errNoToken) {
		v += `, error="invalid_token", error_description="The access token is missing, expired or revoked."`
	}
	w.Header().Set("WWW-Authenticate", v)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

// ---------- tool list ----------

type toolEntry struct {
	tool       ai.Tool
	descriptor map[string]any
}

func (s *Server) toolList() []toolEntry {
	s.toolsOnce.Do(func() {
		if s.ai == nil {
			return
		}
		for _, t := range s.ai.ExternalTools() {
			s.tools = append(s.tools, toolEntry{tool: t, descriptor: describe(t)})
		}
	})
	return s.tools
}

func describe(t ai.Tool) map[string]any {
	scope := scopeRead
	if t.Mutating {
		scope = scopeWrite
	}
	schemes := []map[string]any{{"type": "oauth2", "scopes": []string{scope}}}
	title := strings.ReplaceAll(t.Name, "_", " ")
	title = strings.ToUpper(title[:1]) + title[1:]
	return map[string]any{
		"name":        t.Name,
		"title":       title,
		"description": t.Description,
		"inputSchema": inputSchema(t.Schema),
		"annotations": map[string]any{
			"title":           title,
			"readOnlyHint":    !t.Mutating,
			"destructiveHint": t.Mutating && strings.HasPrefix(t.Name, "delete_"),
			"openWorldHint":   t.Name == "tmdb_search",
		},
		"securitySchemes": schemes,
		"_meta":           map[string]any{"securitySchemes": schemes},
	}
}

// inputSchema turns a Gemini schema into JSON Schema. The top level is
// always an object.
func inputSchema(sc *genai.Schema) map[string]any {
	out := jsonSchema(sc)
	out["type"] = "object"
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]any{}
	}
	return out
}

func jsonSchema(sc *genai.Schema) map[string]any {
	out := map[string]any{}
	if sc == nil {
		return out
	}
	if sc.Type != "" && sc.Type != genai.TypeUnspecified {
		out["type"] = strings.ToLower(string(sc.Type))
	}
	if sc.Description != "" {
		out["description"] = sc.Description
	}
	if len(sc.Enum) > 0 {
		out["enum"] = sc.Enum
	}
	if sc.Format != "" {
		out["format"] = sc.Format
	}
	if sc.Items != nil {
		out["items"] = jsonSchema(sc.Items)
	}
	if len(sc.Properties) > 0 {
		props := map[string]any{}
		for k, v := range sc.Properties {
			props[k] = jsonSchema(v)
		}
		out["properties"] = props
	}
	if len(sc.Required) > 0 {
		out["required"] = sc.Required
	}
	return out
}
