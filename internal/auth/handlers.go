package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"sajni/internal/auth/providers"
)

// makeOAuthState returns a stateless, tamper-evident OAuth state value.
//
// Cookie-based state breaks under aggressive third-party cookie policies
// (Chrome's tracking protection, Safari's ITP, partitioned-storage
// modes). When the API and the web app live on different eTLD+1s,
// SameSite=None cookies set on the API host during /start are dropped
// before /callback runs, so the cookie compare always fails with
// `state mismatch`.
//
// Instead we encode the state as `<nonce>.<expUnix>.<hmac>` where the
// HMAC is computed with JWT_SECRET over the first two segments. The
// callback verifies the signature and the expiry — no server-side
// storage, no cookie. The state is single-use *per OAuth flow* because
// Google won't replay an authorization code, and replay of the state
// alone gets us nothing without the matching code.
func (s *Service) makeOAuthState() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	nonce := base64.RawURLEncoding.EncodeToString(raw)
	exp := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	body := nonce + "." + exp
	h := hmac.New(sha256.New, s.JWTSecret)
	h.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	return body + "." + sig, nil
}

// verifyOAuthState checks the signature and expiry of a state value
// produced by makeOAuthState. Returns nil iff the value is authentic
// and unexpired.
func (s *Service) verifyOAuthState(state string) error {
	parts := strings.Split(state, ".")
	if len(parts) != 3 {
		return errors.New("bad state format")
	}
	body := parts[0] + "." + parts[1]
	h := hmac.New(sha256.New, s.JWTSecret)
	h.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return errors.New("state signature mismatch")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return errors.New("bad state expiry")
	}
	if time.Now().Unix() > exp {
		return errors.New("state expired")
	}
	return nil
}

// RegisterRoutes attaches all unauthenticated auth endpoints to the
// provided mux. /api/auth/me is mounted by the protected router.
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/google/start", s.oauthStart("google"))
	mux.HandleFunc("GET /api/auth/google/callback", s.oauthCallback("google"))
	mux.HandleFunc("GET /api/auth/github/start", s.oauthStart("github"))
	mux.HandleFunc("GET /api/auth/github/callback", s.oauthCallback("github"))
	mux.HandleFunc("POST /api/auth/email/start", s.handleEmailStart)
	mux.HandleFunc("POST /api/auth/email/verify", s.handleEmailVerify)
	mux.HandleFunc("POST /api/auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
}

// userResponse mirrors the shape /me + auth-success bodies use.
type userResponse struct {
	ID          string             `json:"id"`
	Email       string             `json:"email"`
	Name        string             `json:"name"`
	OnboardedAt *string            `json:"onboarded_at"`
	Identities  []identityResponse `json:"identities"`
	DeletedAt   *string            `json:"deleted_at,omitempty"`
}

type identityResponse struct {
	Provider string `json:"provider"`
}

type authResponse struct {
	AccessToken string       `json:"access_token"`
	User        userResponse `json:"user"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// loadUser fills userResponse from a single users row + identities join.
func (s *Service) loadUser(ctx context.Context, id string) (*userResponse, error) {
	var (
		email, name string
		onboarded   sql.NullTime
		deleted     sql.NullTime
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT email, name, onboarded_at, deleted_at FROM users WHERE id=$1`, id,
	).Scan(&email, &name, &onboarded, &deleted)
	if err != nil {
		return nil, err
	}
	resp := &userResponse{ID: id, Email: email, Name: name, Identities: []identityResponse{}}
	if onboarded.Valid {
		v := onboarded.Time.UTC().Format(time.RFC3339)
		resp.OnboardedAt = &v
	}
	if deleted.Valid {
		v := deleted.Time.UTC().Format(time.RFC3339)
		resp.DeletedAt = &v
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT provider FROM auth_identities WHERE user_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		resp.Identities = append(resp.Identities, identityResponse{Provider: p})
	}
	return resp, nil
}

// issueSession mints access + refresh tokens, sets the cookie, and
// returns the auth response body.
func (s *Service) issueSession(ctx context.Context, w http.ResponseWriter, userID string) (*authResponse, error) {
	access, err := s.issueAccessToken(userID)
	if err != nil {
		return nil, fmt.Errorf("issue access token: %w", err)
	}
	refresh, err := s.issueRefreshToken(userID)
	if err != nil {
		return nil, fmt.Errorf("issue refresh token: %w", err)
	}
	s.setRefreshCookie(w, refresh)
	u, err := s.loadUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	return &authResponse{AccessToken: access, User: *u}, nil
}

// ─── OAuth ────────────────────────────────────────────────────────────

func (s *Service) provider(name string) providers.Provider {
	redirect := s.APIBase + "/api/auth/" + name + "/callback"
	switch name {
	case "google":
		return &providers.Google{
			ClientID: s.GoogleClientID, ClientSecret: s.GoogleClientSecret, RedirectURI: redirect,
		}
	case "github":
		return &providers.GitHub{
			ClientID: s.GithubClientID, ClientSecret: s.GithubClientSecret, RedirectURI: redirect,
		}
	}
	return nil
}

// oauthStart issues a stateless HMAC-signed `state` and 302s the
// browser to the provider consent screen. No cookie — see the comment
// on makeOAuthState for the rationale.
func (s *Service) oauthStart(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := s.provider(name)
		if p == nil {
			writeErr(w, http.StatusBadRequest, "unknown provider")
			return
		}
		state, err := s.makeOAuthState()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		http.Redirect(w, r, p.StartURL(state), http.StatusFound)
	}
}

// oauthCallback exchanges the code for an Identity, links/creates the
// user via resolveOrLinkIdentity, then 302s the browser to APP_URL with
// the access token in the fragment so it never hits server logs.
func (s *Service) oauthCallback(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := s.provider(name)
		if p == nil {
			writeErr(w, http.StatusBadRequest, "unknown provider")
			return
		}
		state := r.URL.Query().Get("state")
		if err := s.verifyOAuthState(state); err != nil {
			writeErr(w, http.StatusBadRequest, "state mismatch: "+err.Error())
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			writeErr(w, http.StatusBadRequest, "missing code")
			return
		}
		ident, err := p.Exchange(r.Context(), code)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if ident.Email == "" {
			writeErr(w, http.StatusBadRequest, "provider returned no email")
			return
		}
		userID, needsLink, linkedNew, err := s.resolveOrLinkIdentity(r.Context(), name, ident.Subject, ident.Email, ident.EmailVerified, ident.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if needsLink {
			if err := s.sendEmailCode(r.Context(), ident.Email, ident.Name, "link", userID, name, ident.Subject, ident.Name); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			q := url.Values{}
			q.Set("email", ident.Email)
			q.Set("provider", name)
			http.Redirect(w, r, s.AppURL+"/auth/link?"+q.Encode(), http.StatusFound)
			return
		}
		resp, err := s.issueSession(r.Context(), w, userID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Query string carries the "we just linked X" hint so the
		// frontend can fire a toast. The access token goes in the URL
		// fragment so it never hits server logs or the Referer header.
		dest := s.AppURL + "/auth/done"
		if linkedNew {
			dest += "?linked=" + url.QueryEscape(name)
		}
		dest += "#access=" + url.QueryEscape(resp.AccessToken)
		http.Redirect(w, r, dest, http.StatusFound)
	}
}

func (s *Service) sameSite() http.SameSite {
	if s.CookieInsecure {
		return http.SameSiteLaxMode
	}
	return http.SameSiteNoneMode
}

// ─── Email TOTP ───────────────────────────────────────────────────────

type emailStartBody struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (s *Service) handleEmailStart(w http.ResponseWriter, r *http.Request) {
	var body emailStartBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" || !strings.Contains(email, "@") {
		writeErr(w, http.StatusBadRequest, "valid email required")
		return
	}
	name := strings.TrimSpace(body.Name)
	if err := s.sendEmailCode(r.Context(), email, name, "login", "", "", "", name); err != nil {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "email": email})
}

type emailVerifyBody struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

func (s *Service) handleEmailVerify(w http.ResponseWriter, r *http.Request) {
	var body emailVerifyBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	code := strings.TrimSpace(body.Code)
	if email == "" || code == "" {
		writeErr(w, http.StatusBadRequest, "email and code required")
		return
	}
	consumed, err := s.consumeEmailCode(r.Context(), email, code)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var userID string
	switch consumed.Purpose {
	case "link":
		// Link an OAuth identity (held in the email_codes row) to the
		// existing user_id. The provider previously refused to link
		// because it couldn't vouch for the email.
		userID = consumed.LinkUserID
		if _, err := s.DB.ExecContext(r.Context(),
			`INSERT INTO auth_identities (id, user_id, provider, provider_subject, email, last_used_at)
			 VALUES ($1, $2, $3, $4, $5, NOW())
			 ON CONFLICT (provider, provider_subject) DO UPDATE SET last_used_at = NOW()`,
			NewID(), userID, consumed.LinkProvider, consumed.LinkSubject, email,
		); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	default: // "login"
		// First-class email sign-in: create or fetch by email and ensure
		// an 'email' provider identity row exists.
		uid, _, _, err := s.findOrCreateByEmail(r.Context(), email, consumed.LinkName)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		userID = uid
		// Use distinct positional params for provider_subject (TEXT) and
		// email (CITEXT). Reusing $3 for both columns caused pgx to fail
		// with "inconsistent types deduced for parameter $3 (SQLSTATE
		// 42P08)" because Postgres tries to settle on a single type for
		// each placeholder.
		if _, err := s.DB.ExecContext(r.Context(),
			`INSERT INTO auth_identities (id, user_id, provider, provider_subject, email, last_used_at)
			 VALUES ($1, $2, 'email', $3, $4, NOW())
			 ON CONFLICT (provider, provider_subject) DO UPDATE SET last_used_at = NOW()`,
			NewID(), userID, email, email,
		); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	resp, err := s.issueSession(r.Context(), w, userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── Refresh / Logout ────────────────────────────────────────────────

func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookie)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "no refresh token")
		return
	}
	userID, err := s.consumeRefreshToken(cookie.Value)
	if err != nil {
		// Don't clear the cookie here. A single failed consume can happen
		// during a benign race (two tabs, StrictMode double-mount, a
		// network hiccup that retries), and nuking the cookie permanently
		// logs the user out for no good reason. Only /auth/logout clears
		// the cookie explicitly. On the next user-initiated sign-in a
		// fresh cookie is issued anyway.
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	resp, err := s.issueSession(r.Context(), w, userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookie)
	if err == nil && cookie.Value != "" {
		_, _ = s.consumeRefreshToken(cookie.Value)
	}
	s.clearRefreshCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleMe is protected (mounted on the auth-middleware mux) and returns
// the current user with their linked identities.
func (s *Service) HandleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := UserIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := s.loadUser(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusUnauthorized, "user not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// HandleUpdateProfile lets the user edit their display name from the
// Settings page. We only expose name today — email is the identity
// anchor and provider identities are append-only.
func (s *Service) HandleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	id := MustUserID(r.Context())
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	if len(name) > 120 {
		name = name[:120]
	}
	if _, err := s.DB.ExecContext(r.Context(),
		`UPDATE users SET name=$2 WHERE id=$1`, id, name,
	); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	u, err := s.loadUser(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// HandleOnboarded marks the user's walkthrough complete. Idempotent —
// repeat calls keep the first timestamp.
func (s *Service) HandleOnboarded(w http.ResponseWriter, r *http.Request) {
	id := MustUserID(r.Context())
	if _, err := s.DB.ExecContext(r.Context(),
		`UPDATE users SET onboarded_at = COALESCE(onboarded_at, NOW()) WHERE id = $1`, id,
	); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
