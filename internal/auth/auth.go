// Package auth implements Sajni's authentication: three sign-in
// methods (Google OAuth, GitHub OAuth, email + Resend-delivered TOTP)
// linked by verified email, JWT access tokens, and SHA-256-hashed
// rotating refresh tokens stored by indexed lookup (no bcrypt sweep).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"sajni/internal/db"
)

// ContextKey is the type used to store the user ID (UUID string) in
// request contexts.
type ContextKey struct{}

const (
	// 30 minutes — bumped from 15. Halves refresh churn while still
	// short enough to expire stolen tokens quickly.
	accessTokenTTL  = 30 * time.Minute
	refreshTokenTTL = 7 * 24 * time.Hour
	refreshCookie   = "sajni_refresh"
)

// Service holds the dependencies the auth handlers need.
type Service struct {
	DB        *db.DB
	JWTSecret []byte
	// CookieInsecure tells the service to issue refresh cookies without
	// the Secure flag and with SameSite=Lax (for HTTP localhost dev).
	CookieInsecure bool
	// AppURL is the public origin of the web frontend (e.g.
	// "https://ohmysajni.com"). Used for OAuth callback redirects.
	AppURL string
	// APIBase is the public origin of this API (e.g.
	// "https://api.ohmysajni.com"). Used to construct OAuth callback URLs.
	APIBase string

	// OAuth credentials
	GoogleClientID     string
	GoogleClientSecret string
	GithubClientID     string
	GithubClientSecret string

	// Resend
	ResendAPIKey string
	EmailFrom    string
	// DevLogEmailCodes permits the local email fallback to print one-time
	// codes. It must be explicitly enabled; production fails closed.
	DevLogEmailCodes bool

	// DevAuthBypass mints a normal session from /auth/refresh for
	// local/private-origin requests. It is intentionally env-gated and
	// request-gated so setting it accidentally in production is not enough
	// to disable auth for public traffic.
	DevAuthBypass bool
	DevAuthEmail  string
	DevAuthName   string
}

// NewService reads required env, returns an error if anything load-bearing
// is missing.
func NewService(database *db.DB) (*Service, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return nil, errors.New("JWT_SECRET is required")
	}
	appURL := strings.TrimRight(os.Getenv("APP_URL"), "/")
	if appURL == "" {
		appURL = "http://localhost:5173"
	}
	apiBase := strings.TrimRight(os.Getenv("API_BASE_URL"), "/")
	if apiBase == "" {
		apiBase = "http://localhost:8080"
	}
	devAuthEmail := strings.ToLower(strings.TrimSpace(os.Getenv("DEV_AUTH_BYPASS_EMAIL")))
	if devAuthEmail == "" {
		devAuthEmail = "dev@sajni.local"
	}
	devAuthName := strings.TrimSpace(os.Getenv("DEV_AUTH_BYPASS_NAME"))
	if devAuthName == "" {
		devAuthName = "Sajni Dev"
	}
	resendAPIKey := os.Getenv("RESEND_API_KEY")
	devLogEmailCodes := os.Getenv("AUTH_DEV_CODE_LOG") == "1"
	if resendAPIKey == "" && !devLogEmailCodes {
		return nil, errors.New("RESEND_API_KEY is required unless AUTH_DEV_CODE_LOG=1")
	}
	return &Service{
		DB:                 database,
		JWTSecret:          []byte(secret),
		CookieInsecure:     os.Getenv("COOKIE_INSECURE") == "1",
		AppURL:             appURL,
		APIBase:            apiBase,
		GoogleClientID:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		GithubClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		GithubClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		ResendAPIKey:       resendAPIKey,
		EmailFrom:          os.Getenv("EMAIL_FROM"),
		DevLogEmailCodes:   devLogEmailCodes,
		DevAuthBypass:      os.Getenv("DEV_AUTH_BYPASS") == "1",
		DevAuthEmail:       devAuthEmail,
		DevAuthName:        devAuthName,
	}, nil
}

// UserIDFromContext extracts the authenticated user id (UUID string), if any.
func UserIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ContextKey{}).(string)
	return id, ok
}

// MustUserID panics if no user id is in context — indicates a routing
// bug, since auth middleware should have run.
func MustUserID(ctx context.Context) string {
	id, ok := UserIDFromContext(ctx)
	if !ok {
		panic("auth: user id missing from context")
	}
	return id
}

type accessClaims struct {
	jwt.RegisteredClaims
}

func (s *Service) issueAccessToken(userID string) (string, error) {
	now := time.Now()
	claims := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(s.JWTSecret)
}

func (s *Service) parseAccessToken(raw string) (string, error) {
	tok, err := jwt.ParseWithClaims(raw, &accessClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.JWTSecret, nil
	})
	if err != nil {
		return "", err
	}
	c, ok := tok.Claims.(*accessClaims)
	if !ok || !tok.Valid {
		return "", errors.New("invalid token")
	}
	if _, err := uuid.Parse(c.Subject); err != nil {
		return "", fmt.Errorf("bad subject: %w", err)
	}
	return c.Subject, nil
}

// Random URL-safe token for refresh cookies.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken returns the SHA-256 digest of a refresh token. Stored as
// BYTEA with a UNIQUE index for O(1) lookup on the refresh hot path.
func hashToken(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

// issueRefreshToken creates a new refresh token, stores its SHA-256 hash
// in a UUID-PK row, and returns the raw token for the cookie.
func (s *Service) issueRefreshToken(userID string) (string, error) {
	raw, err := randomToken(32)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.Exec(
		"INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, $4)",
		NewID(), userID, hashToken(raw), time.Now().Add(refreshTokenTTL),
	); err != nil {
		return "", err
	}
	return raw, nil
}

// consumeRefreshToken atomically deletes the matching row (by SHA-256
// hash + non-expired) and returns the user id. One indexed lookup, no
// bcrypt — the previous implementation bcrypt-compared every active
// row, which is what made /refresh slow.
func (s *Service) consumeRefreshToken(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("missing refresh token")
	}
	var userID string
	err := s.DB.QueryRow(
		`DELETE FROM refresh_tokens
		 WHERE token_hash = $1 AND expires_at > NOW()
		 RETURNING user_id`,
		hashToken(raw),
	).Scan(&userID)
	if err != nil {
		return "", errors.New("invalid refresh token")
	}
	return userID, nil
}

// Middleware wraps an HTTP handler and rejects requests that don't carry
// a valid Bearer access token.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")
		userID, err := s.parseAccessToken(token)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ContextKey{}, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// setRefreshCookie writes the refresh token cookie with the right flags
// for the current environment.
func (s *Service) setRefreshCookie(w http.ResponseWriter, raw string) {
	cookie := &http.Cookie{
		Name:     refreshCookie,
		Value:    raw,
		Path:     "/api/auth",
		HttpOnly: true,
		Expires:  time.Now().Add(refreshTokenTTL),
	}
	if s.CookieInsecure {
		cookie.SameSite = http.SameSiteLaxMode
		cookie.Secure = false
	} else {
		cookie.SameSite = http.SameSiteNoneMode
		cookie.Secure = true
	}
	http.SetCookie(w, cookie)
}

func (s *Service) clearRefreshCookie(w http.ResponseWriter) {
	cookie := &http.Cookie{
		Name:     refreshCookie,
		Value:    "",
		Path:     "/api/auth",
		HttpOnly: true,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	}
	if s.CookieInsecure {
		cookie.SameSite = http.SameSiteLaxMode
	} else {
		cookie.SameSite = http.SameSiteNoneMode
		cookie.Secure = true
	}
	http.SetCookie(w, cookie)
}
