package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/config"
	"sajni/internal/db"
	"sajni/internal/reminderqueue"
)

func TestRedirectRules(t *testing.T) {
	doc := &client{ID: "https://chatgpt.com/oauth/client.json"}
	dcr := &client{ID: "dcr_x", RedirectURIs: []string{"https://app.example/cb", "http://127.0.0.1:3000/callback"}}
	for _, tc := range []struct {
		c    *client
		uri  string
		want bool
	}{
		{doc, "https://chatgpt.com/connector_platform_oauth_redirect", true},
		{doc, "https://chatgpt.com/connector/oauth/abc", true},
		{doc, "https://evil.example/cb", false},
		{doc, "http://chatgpt.com/cb", false},
		{dcr, "https://app.example/cb", true},
		{dcr, "https://app.example/cb2", false},
		{dcr, "http://127.0.0.1:49152/callback", true}, // any loopback port
		{dcr, "http://127.0.0.1:49152/other", false},
		{dcr, "https://app.example/cb#frag", false},
	} {
		if got := tc.c.redirectAllowed(tc.uri); got != tc.want {
			t.Errorf("%s → %s = %v, want %v", tc.c.ID, tc.uri, got, tc.want)
		}
	}
}

func TestSchemaConversion(t *testing.T) {
	svc, _ := ai.NewService(context.Background(), nil, nil, config.AI{GeminiAPIKey: "test", GeminiModel: "m"}, config.Media{}, reminderqueue.Queue{})
	var listTasks map[string]any
	for _, tool := range svc.ExternalTools() {
		if tool.Name == "generate_theme" {
			t.Fatal("app-only tool exposed")
		}
		d := describe(tool)
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"STRING"`) || strings.Contains(string(b), `"OBJECT"`) {
			t.Fatalf("%s kept Gemini type names: %s", tool.Name, b)
		}
		if tool.Name == "list_tasks" {
			listTasks = d
		}
	}
	in := listTasks["inputSchema"].(map[string]any)
	if in["type"] != "object" || in["properties"].(map[string]any)["status"].(map[string]any)["type"] != "string" {
		t.Fatalf("list_tasks schema %v", in)
	}
	if listTasks["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatal("list_tasks should be read-only")
	}
}

// The whole connection: register, approve, swap the code, call tools,
// refresh, disconnect. Needs a scratch Postgres:
// SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/mcp
func TestConnectFlow(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000b2"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'mcp@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO tasks (user_id, title, status) VALUES ($1, 'Pay rent', 'todo')`, uid)

	svc, err := ai.NewService(context.Background(), d, nil, config.AI{GeminiAPIKey: "test", GeminiModel: "m"}, config.Media{}, reminderqueue.Queue{})
	if err != nil {
		t.Fatal(err)
	}
	s := New(d, svc, "https://www.ohmysajni.com")
	pub, user := http.NewServeMux(), http.NewServeMux()
	s.RegisterPublic(pub)
	s.RegisterUser(user)
	asUser := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.ContextKey{}, uid)))
	})
	do := func(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	form := func(path string, v url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return do(pub, r)
	}
	decode := func(w *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}

	// Unauthenticated MCP call → 401 pointing at the resource metadata.
	w := do(pub, httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)))
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("challenge %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	if m := decode(do(pub, httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil))); m["client_id_metadata_document_supported"] != true {
		t.Fatalf("AS metadata %v", m)
	}

	// Register a public client.
	w = do(pub, httptest.NewRequest("POST", "/api/oauth/register", strings.NewReader(
		`{"client_name":"Test GPT","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none"}`)))
	reg := decode(w)
	clientID, _ := reg["client_id"].(string)
	if w.Code != 201 || clientID == "" || reg["client_secret"] != nil {
		t.Fatalf("register %d %v", w.Code, reg)
	}
	defer d.Exec(`DELETE FROM mcp_clients WHERE id = $1`, clientID)

	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://app.example/cb"},
		"state": {"xyz"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"}, "scope": {"read write"}, "resource": {"https://www.ohmysajni.com/mcp"},
	}
	info := decode(do(asUser, httptest.NewRequest("GET", "/api/oauth/request?"+q.Encode(), nil)))
	if info["client_name"] != "Test GPT" || info["write_requested"] != true || info["connected"] != false {
		t.Fatalf("consent info %v", info)
	}
	// A redirect URI the client didn't register is refused outright.
	bad := url.Values{}
	for k, v := range q {
		bad[k] = v
	}
	bad.Set("redirect_uri", "https://evil.example/cb")
	if w := do(asUser, httptest.NewRequest("GET", "/api/oauth/request?"+bad.Encode(), nil)); w.Code != 400 {
		t.Fatalf("bad redirect %d", w.Code)
	}

	// Approve read-only first.
	approve := func(write bool) string {
		b, _ := json.Marshal(map[string]any{"query": q.Encode(), "allow": true, "write": write})
		res := decode(do(asUser, httptest.NewRequest("POST", "/api/oauth/approve", strings.NewReader(string(b)))))
		u, err := url.Parse(res["redirect"].(string))
		if err != nil || u.Query().Get("state") != "xyz" || u.Query().Get("iss") != "https://www.ohmysajni.com" {
			t.Fatalf("approve %v", res)
		}
		return u.Query().Get("code")
	}
	exchange := func(code string) map[string]any {
		w := form("/api/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
			"redirect_uri": {"https://app.example/cb"}, "code_verifier": {verifier}, "resource": {"https://www.ohmysajni.com/mcp"}})
		if w.Code != 200 {
			t.Fatalf("token %d %s", w.Code, w.Body)
		}
		return decode(w)
	}
	code := approve(false)
	if w := form("/api/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"code_verifier": {strings.Repeat("x", 50)}}); w.Code != 400 {
		t.Fatalf("wrong verifier accepted: %d", w.Code)
	}
	// The failed try used the code up.
	if w := form("/api/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"code_verifier": {verifier}}); w.Code != 400 {
		t.Fatalf("code reused: %d", w.Code)
	}
	tok := exchange(approve(false))
	if tok["scope"] != "read" {
		t.Fatalf("scope %v", tok["scope"])
	}

	rpc := func(access string, body string) (int, map[string]any) {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+access)
		w := do(pub, r)
		return w.Code, decode(w)
	}
	toolNames := func(access string) []string {
		_, m := rpc(access, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		var names []string
		for _, t := range m["result"].(map[string]any)["tools"].([]any) {
			names = append(names, t.(map[string]any)["name"].(string))
		}
		return names
	}
	access := tok["access_token"].(string)
	if code, m := rpc(access, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`); code != 200 ||
		m["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize %d %v", code, m)
	}
	if code, _ := rpc(access, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Fatalf("notification %d", code)
	}
	names := strings.Join(toolNames(access), ",")
	if !strings.Contains(names, "list_tasks") || strings.Contains(names, "create_task") {
		t.Fatalf("read-only tools: %s", names)
	}
	_, m := rpc(access, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_tasks","arguments":{"status":"open"}}}`)
	text := m["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Pay rent") {
		t.Fatalf("list_tasks → %s", text)
	}
	_, m = rpc(access, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"create_task","arguments":{"title":"x"}}}`)
	if m["result"].(map[string]any)["isError"] != true {
		t.Fatalf("read-only write allowed: %v", m)
	}

	// Approving again with changes allowed widens the same connection.
	tok = exchange(approve(true))
	access = tok["access_token"].(string)
	if !strings.Contains(strings.Join(toolNames(access), ","), "create_task") {
		t.Fatal("write tools missing")
	}

	// Refresh: new pair; the old refresh token still works briefly.
	refresh := tok["refresh_token"].(string)
	for range 2 {
		if w := form("/api/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); w.Code != 200 {
			t.Fatalf("refresh %d %s", w.Code, w.Body)
		}
	}

	// Settings lists it; removing it signs the app out at once.
	w = do(asUser, httptest.NewRequest("GET", "/api/connected-apps", nil))
	var apps []map[string]any
	json.Unmarshal(w.Body.Bytes(), &apps)
	if len(apps) != 1 || apps[0]["name"] != "Test GPT" || apps[0]["can_write"] != true || apps[0]["host"] != "app.example" {
		t.Fatalf("apps %s", w.Body)
	}
	if w := do(asUser, httptest.NewRequest("DELETE", "/api/connected-apps/"+apps[0]["id"].(string), nil)); w.Code != 200 {
		t.Fatalf("remove %d", w.Code)
	}
	if code, _ := rpc(access, `{"jsonrpc":"2.0","id":5,"method":"ping"}`); code != 401 {
		t.Fatalf("token alive after removal: %d", code)
	}
}
