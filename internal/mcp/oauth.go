package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/auth"
)

const (
	codeTTL    = 10 * time.Minute
	accessTTL  = time.Hour
	refreshTTL = 90 * 24 * time.Hour
	// A used refresh token keeps working this long, so a client that
	// retries a refresh (or sends two at once) isn't signed out.
	refreshGrace = 2 * time.Minute
	// Client metadata documents are read again after this long.
	metadataTTL = 24 * time.Hour
)

// client is an app that may ask for access.
type client struct {
	ID           string
	Name         string
	RedirectURIs []string
	SecretHash   []byte // nil: public client
}

func (c *client) isDocument() bool { return strings.HasPrefix(c.ID, "https://") }

// host is what the consent page shows as the app's address.
func (c *client) host() string {
	if c.isDocument() {
		if u, err := url.Parse(c.ID); err == nil {
			return u.Host
		}
	}
	if len(c.RedirectURIs) > 0 {
		if u, err := url.Parse(c.RedirectURIs[0]); err == nil {
			return u.Host
		}
	}
	return ""
}

// redirectAllowed: a registered redirect URI exactly; any port on a
// loopback one (RFC 8252); and for a metadata document, any https URL on
// the document's own origin (ChatGPT's document doesn't list its callback).
func (c *client) redirectAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil {
		return false
	}
	for _, r := range c.RedirectURIs {
		if r == raw {
			return true
		}
		if ru, err := url.Parse(r); err == nil && isLoopback(ru) && isLoopback(u) &&
			ru.Scheme == u.Scheme && ru.Hostname() == u.Hostname() && ru.Path == u.Path {
			return true
		}
	}
	if c.isDocument() && u.Scheme == "https" {
		if cu, err := url.Parse(c.ID); err == nil && strings.EqualFold(cu.Host, u.Host) {
			return true
		}
	}
	return false
}

func isLoopback(u *url.URL) bool {
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// loadClient reads a client, fetching (or re-reading) a metadata document.
func (s *Server) loadClient(ctx context.Context, id string, fetch bool) (*client, error) {
	c := &client{ID: id}
	var fetched sql.NullTime
	var uris []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT name, redirect_uris, secret_hash, fetched_at FROM mcp_clients WHERE id=$1`, id).
		Scan(&c.Name, &uris, &c.SecretHash, &fetched)
	json.Unmarshal(uris, &c.RedirectURIs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	found := err == nil
	if !c.isDocument() || !fetch || (found && fetched.Valid && time.Since(fetched.Time) < metadataTTL) {
		if !found {
			return nil, errors.New("unknown client")
		}
		return c, nil
	}
	doc, ferr := s.readMetadata(ctx, id)
	if ferr != nil {
		if found {
			log.Warn().Err(ferr).Str("client", id).Msg("mcp client document refresh failed; using the saved copy")
			return c, nil
		}
		return nil, ferr
	}
	c.Name, c.RedirectURIs, c.SecretHash = doc.name(), doc.RedirectURIs, nil
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mcp_clients (id, name, redirect_uris, fetched_at) VALUES ($1,$2,$3::jsonb,NOW())
		ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, redirect_uris=EXCLUDED.redirect_uris, fetched_at=NOW()`,
		id, c.Name, jsonList(c.RedirectURIs))
	return c, err
}

// ---------- client metadata documents ----------

type metadataDoc struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	ClientURI    string   `json:"client_uri"`
	RedirectURIs []string `json:"redirect_uris"`
}

func (d *metadataDoc) name() string {
	if n := strings.TrimSpace(d.ClientName); n != "" {
		return clip(n, 100)
	}
	u, _ := url.Parse(d.ClientID)
	return u.Host
}

// metadataClient refuses to connect to private or loopback addresses, so a
// client id can't point the server at internal services.
func metadataClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
				ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				return errors.New("address not allowed")
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (s *Server) readMetadata(ctx context.Context, id string) (*metadataDoc, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" ||
		u.Path == "" || u.Path == "/" || strings.Contains(u.Path, "/./") || strings.Contains(u.Path, "/../") {
		return nil, errors.New("client id is not a valid metadata URL")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Sajni/1.0 (+https://www.ohmysajni.com)")
	resp, err := s.fetch.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read client document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("client document answered %d", resp.StatusCode)
	}
	var doc metadataDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("client document: %w", err)
	}
	if doc.ClientID != id {
		return nil, errors.New("client document names a different client_id")
	}
	return &doc, nil
}

// ---------- dynamic client registration (RFC 7591) ----------

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.reg.allow() {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "Too many registrations; try again later.")
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		GrantTypes   []string `json:"grant_types"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "Body must be JSON.")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "Give one to ten redirect_uris.")
		return
	}
	for _, raw := range req.RedirectURIs {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil || !(u.Scheme == "https" || isLoopback(u)) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "Redirect URIs must be https, or http on localhost.")
			return
		}
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "Only authorization_code and refresh_token are supported.")
			return
		}
	}
	method := req.AuthMethod
	switch method {
	case "none", "client_secret_post", "client_secret_basic":
	case "":
		method = "client_secret_basic" // the RFC 7591 default
	default:
		method = "none" // e.g. private_key_jwt: fall back to PKCE alone
	}
	name := clip(strings.TrimSpace(req.ClientName), 100)
	if name == "" {
		u, _ := url.Parse(req.RedirectURIs[0])
		name = u.Host
	}

	ctx := r.Context()
	// Clear out registrations nobody approved.
	s.db.ExecContext(ctx, `DELETE FROM mcp_clients c WHERE c.id LIKE 'dcr\_%' AND c.created_at < NOW() - INTERVAL '1 day'
		AND NOT EXISTS (SELECT 1 FROM mcp_grants g WHERE g.client_id = c.id)`)

	id := "dcr_" + randomToken(16)
	var secret string
	var secretHash []byte
	if method != "none" {
		secret = randomToken(32)
		secretHash = hash(secret)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_clients (id, name, redirect_uris, secret_hash) VALUES ($1,$2,$3::jsonb,$4)`,
		id, name, jsonList(req.RedirectURIs), secretHash); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "Couldn't save the client.")
		return
	}
	log.Info().Str("client", id).Str("name", name).Str("auth_method", method).Msg("mcp client registered")
	out := map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"scope":                      scopeRead + " " + scopeWrite,
	}
	if secret != "" {
		out["client_secret"] = secret
		out["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, out)
}

// ---------- authorization (the consent page's API) ----------

// authRequest is a checked /oauth/authorize request.
type authRequest struct {
	client      *client
	redirectURI string
	state       string
	challenge   string
	wantsWrite  bool
}

// parseAuthorize checks an authorize query. A bad client or redirect URI
// is fatal (never redirect to an unchecked URL); other problems come back
// as an error redirect for the app.
func (s *Server) parseAuthorize(ctx context.Context, q url.Values) (req *authRequest, errRedirect string, fatal error) {
	id := q.Get("client_id")
	if id == "" {
		return nil, "", errors.New("The link is missing which app is asking.")
	}
	c, err := s.loadClient(ctx, id, true)
	if err != nil {
		log.Warn().Err(err).Str("client", id).Msg("mcp authorize: client")
		return nil, "", errors.New("This app isn't registered with Sajni. Remove it and add it again.")
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}
	if redirect == "" || !c.redirectAllowed(redirect) {
		return nil, "", errors.New("This app asked to return to an address it didn't register.")
	}
	state := q.Get("state")
	fail := func(code, desc string) (*authRequest, string, error) {
		return nil, s.redirectWith(redirect, url.Values{"error": {code}, "error_description": {desc}}, state), nil
	}
	switch {
	case q.Get("response_type") != "code":
		return fail("unsupported_response_type", "Only response_type=code is supported.")
	case q.Get("code_challenge") == "":
		return fail("invalid_request", "PKCE is required: send code_challenge with method S256.")
	case q.Get("code_challenge_method") != "S256":
		return fail("invalid_request", "code_challenge_method must be S256.")
	case !s.resourceOK(q.Get("resource")):
		return fail("invalid_target", "This server only issues tokens for "+s.resourceURL()+".")
	}
	wantsWrite := true
	if sc := strings.Fields(q.Get("scope")); len(sc) > 0 {
		wantsWrite = false
		known := false
		for _, v := range sc {
			known = known || v == scopeRead || v == scopeWrite
			wantsWrite = wantsWrite || v == scopeWrite
		}
		if !known {
			wantsWrite = true // only scopes we don't know (e.g. openid): ask for everything
		}
	}
	return &authRequest{client: c, redirectURI: redirect, state: state, challenge: q.Get("code_challenge"), wantsWrite: wantsWrite}, "", nil
}

// resourceOK: the resource indicator (RFC 8707), when sent, must be /mcp.
func (s *Server) resourceOK(v string) bool {
	if v == "" {
		return true
	}
	return strings.TrimRight(v, "/") == s.resourceURL() || strings.TrimRight(v, "/") == s.base
}

// redirectWith adds params, state and iss (RFC 9207) to the app's redirect URI.
func (s *Server) redirectWith(redirect string, params url.Values, state string) string {
	u, _ := url.Parse(redirect)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.base)
	u.RawQuery = q.Encode()
	return u.String()
}

// consentInfo tells the consent page who is asking.
func (s *Server) consentInfo(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFromContext(r.Context())
	req, redirect, fatal := s.parseAuthorize(r.Context(), r.URL.Query())
	if fatal != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fatal.Error()})
		return
	}
	if redirect != "" {
		writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
		return
	}
	var scope string
	s.db.QueryRowContext(r.Context(), `SELECT scope FROM mcp_grants WHERE user_id=$1 AND client_id=$2`, uid, req.client.ID).Scan(&scope)
	rh := ""
	if u, err := url.Parse(req.redirectURI); err == nil {
		rh = u.Host
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client_name":     req.client.Name,
		"client_host":     req.client.host(),
		"redirect_host":   rh,
		"write_requested": req.wantsWrite,
		"connected":       scope != "",
		"connected_write": strings.Contains(scope, scopeWrite),
	})
}

// approve records the user's answer and returns where to send the browser.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid, _ := auth.UserIDFromContext(ctx)
	var body struct {
		Query string `json:"query"`
		Allow bool   `json:"allow"`
		Write bool   `json:"write"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	q, err := url.ParseQuery(strings.TrimPrefix(body.Query, "?"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	req, redirect, fatal := s.parseAuthorize(ctx, q)
	if fatal != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fatal.Error()})
		return
	}
	if redirect != "" {
		writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
		return
	}
	if !body.Allow {
		writeJSON(w, http.StatusOK, map[string]string{"redirect": s.redirectWith(req.redirectURI,
			url.Values{"error": {"access_denied"}, "error_description": {"You chose not to connect."}}, req.state)})
		return
	}
	scope := scopeRead
	if body.Write && req.wantsWrite {
		scope += " " + scopeWrite
	}
	var grantID string
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO mcp_grants (id, user_id, client_id, scope) VALUES ($1,$2,$3,$4)
		ON CONFLICT (user_id, client_id) DO UPDATE SET scope = EXCLUDED.scope
		RETURNING id`, auth.NewID(), uid, req.client.ID, scope).Scan(&grantID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Couldn't save the connection."})
		return
	}
	code := randomToken(32)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_tokens (token_hash, grant_id, kind, redirect_uri, code_challenge, expires_at)
		VALUES ($1,$2,'code',$3,$4,$5)`,
		hash(code), grantID, req.redirectURI, req.challenge, time.Now().Add(codeTTL)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Couldn't save the connection."})
		return
	}
	log.Info().Str("uid", uid).Str("client", req.client.ID).Str("scope", scope).Msg("mcp app approved")
	writeJSON(w, http.StatusOK, map[string]string{"redirect": s.redirectWith(req.redirectURI, url.Values{"code": {code}}, req.state)})
}

// ---------- token endpoint ----------

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "Send a form-encoded body.")
		return
	}
	ctx := r.Context()
	c, ok := s.authenticateClient(w, r)
	if !ok {
		return
	}
	if !s.resourceOK(r.PostForm.Get("resource")) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "This server only issues tokens for "+s.resourceURL()+".")
		return
	}
	var (
		grantID, clientID, scope string
		expires                  time.Time
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		var redirect, challenge string
		err := s.db.QueryRowContext(ctx, `
			DELETE FROM mcp_tokens t USING mcp_grants g
			WHERE t.token_hash=$1 AND t.kind='code' AND g.id=t.grant_id
			RETURNING t.grant_id, g.client_id, g.scope, t.redirect_uri, t.code_challenge, t.expires_at`,
			hash(r.PostForm.Get("code"))).Scan(&grantID, &clientID, &scope, &redirect, &challenge, &expires)
		if err != nil || time.Now().After(expires) || clientID != c.ID {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "The code is unknown, used or expired.")
			return
		}
		if ru := r.PostForm.Get("redirect_uri"); ru != "" && ru != redirect {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri doesn't match the authorization request.")
			return
		}
		if !pkceOK(r.PostForm.Get("code_verifier"), challenge) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier doesn't match the code_challenge.")
			return
		}
	case "refresh_token":
		// Spend the refresh token but let it work for a short grace, so a
		// retried refresh doesn't sign the app out.
		err := s.db.QueryRowContext(ctx, `
			UPDATE mcp_tokens t SET kind='spent', expires_at = LEAST(t.expires_at, NOW() + $2::interval)
			FROM mcp_grants g
			WHERE t.token_hash=$1 AND t.kind IN ('refresh','spent') AND t.expires_at > NOW() AND g.id=t.grant_id
			RETURNING t.grant_id, g.client_id, g.scope`,
			hash(r.PostForm.Get("refresh_token")), fmt.Sprintf("%d seconds", int(refreshGrace.Seconds()))).
			Scan(&grantID, &clientID, &scope)
		if err != nil || clientID != c.ID {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "The refresh token is unknown, expired or revoked.")
			return
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "Use authorization_code or refresh_token.")
		return
	}

	access, refresh := randomToken(32), randomToken(32)
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_tokens (token_hash, grant_id, kind, expires_at)
		VALUES ($1,$3,'access',$4), ($2,$3,'refresh',$5)`,
		hash(access), hash(refresh), grantID, now.Add(accessTTL), now.Add(refreshTTL))
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "Couldn't issue tokens.")
		return
	}
	s.db.ExecContext(ctx, `DELETE FROM mcp_tokens WHERE expires_at < NOW()`)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	})
}

// authenticateClient checks client_id and, for confidential clients, the
// secret (HTTP Basic or form). It writes the error itself.
func (s *Server) authenticateClient(w http.ResponseWriter, r *http.Request) (*client, bool) {
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id == "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client_id is required.")
		return nil, false
	}
	c, err := s.loadClient(r.Context(), id, false)
	if err != nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "Unknown client.")
		return nil, false
	}
	if c.SecretHash != nil && subtle.ConstantTimeCompare(hash(secret), c.SecretHash) != 1 {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "Wrong client secret.")
		return nil, false
	}
	return c, true
}

// revoke (RFC 7009) forgets one token. Unknown tokens are fine.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "Send a form-encoded body.")
		return
	}
	if t := r.PostForm.Get("token"); t != "" {
		s.db.ExecContext(r.Context(), `DELETE FROM mcp_tokens WHERE token_hash=$1 AND kind <> 'code'`, hash(t))
	}
	w.WriteHeader(http.StatusOK)
}

// ---------- Settings → Connected apps ----------

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFromContext(r.Context())
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT g.id, c.id, c.name, c.redirect_uris, g.scope, g.created_at, g.last_used_at
		FROM mcp_grants g JOIN mcp_clients c ON c.id = g.client_id
		WHERE g.user_id = $1 ORDER BY g.created_at DESC`, uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var c client
		var id, scope string
		var uris []byte
		var created time.Time
		var used sql.NullTime
		if err := rows.Scan(&id, &c.ID, &c.Name, &uris, &scope, &created, &used); err != nil {
			continue
		}
		json.Unmarshal(uris, &c.RedirectURIs)
		app := map[string]any{
			"id":         id,
			"name":       c.Name,
			"host":       c.host(),
			"can_write":  strings.Contains(scope, scopeWrite),
			"created_at": created,
		}
		if used.Valid {
			app["last_used_at"] = used.Time
		}
		out = append(out, app)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) removeApp(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserIDFromContext(r.Context())
	res, err := s.db.ExecContext(r.Context(), `DELETE FROM mcp_grants WHERE id::text=$1 AND user_id=$2`, r.PathValue("id"), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	log.Info().Str("uid", uid).Str("grant", r.PathValue("id")).Msg("mcp app disconnected")
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// ---------- helpers ----------

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="sajni"`)
	}
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	h := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(h[:])), []byte(challenge)) == 1
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// window allows max events per hour.
type window struct {
	mu    sync.Mutex
	max   int
	times []time.Time
}

func newWindow(max int) *window { return &window{max: max} }

func (w *window) allow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cut := time.Now().Add(-time.Hour)
	i := 0
	for i < len(w.times) && w.times[i].Before(cut) {
		i++
	}
	w.times = w.times[i:]
	if len(w.times) >= w.max {
		return false
	}
	w.times = append(w.times, time.Now())
	return true
}
