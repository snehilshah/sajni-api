// Package auth implements email/password authentication with JWT
// access tokens and rotating refresh tokens stored as bcrypt hashes.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"sajni/internal/db"
)

// ContextKey is the type used to store the user ID in request contexts.
type ContextKey struct{}

const (
	accessTokenTTL  = 15 * time.Minute
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
}

// NewService reads JWT_SECRET / COOKIE_INSECURE from the environment.
// Returns an error if no JWT_SECRET is configured.
func NewService(database *db.DB) (*Service, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return nil, errors.New("JWT_SECRET is required")
	}
	insecure := os.Getenv("COOKIE_INSECURE") == "1"
	return &Service{DB: database, JWTSecret: []byte(secret), CookieInsecure: insecure}, nil
}

// UserIDFromContext extracts the authenticated user id, if any.
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ContextKey{}).(int64)
	return id, ok
}

// MustUserID is a convenience for handlers that have already passed
// through Middleware — it panics if the id isn't present, which would
// indicate a routing bug.
func MustUserID(ctx context.Context) int64 {
	id, ok := UserIDFromContext(ctx)
	if !ok {
		panic("auth: user id missing from context")
	}
	return id
}

type accessClaims struct {
	jwt.RegisteredClaims
}

func (s *Service) issueAccessToken(userID int64) (string, error) {
	now := time.Now()
	claims := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprint(userID),
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(s.JWTSecret)
}

func (s *Service) parseAccessToken(raw string) (int64, error) {
	tok, err := jwt.ParseWithClaims(raw, &accessClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.JWTSecret, nil
	})
	if err != nil {
		return 0, err
	}
	c, ok := tok.Claims.(*accessClaims)
	if !ok || !tok.Valid {
		return 0, errors.New("invalid token")
	}
	var id int64
	if _, err := fmt.Sscanf(c.Subject, "%d", &id); err != nil {
		return 0, fmt.Errorf("bad subject: %w", err)
	}
	return id, nil
}

// Random URL-safe token for refresh cookies.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// issueRefreshToken creates a new refresh token, stores its bcrypt hash,
// and returns the raw token string the client should receive in a cookie.
func (s *Service) issueRefreshToken(userID int64) (string, error) {
	raw, err := randomToken(32)
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(raw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.Exec(
		"INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)",
		userID, string(hash), time.Now().Add(refreshTokenTTL),
	); err != nil {
		return "", err
	}
	return raw, nil
}

// consumeRefreshToken finds the (user, token row) pair matching the raw
// token, deletes the row (rotation), and returns the user id. Expired or
// missing tokens yield an error.
func (s *Service) consumeRefreshToken(raw string) (int64, error) {
	if raw == "" {
		return 0, errors.New("missing refresh token")
	}
	rows, err := s.DB.Query(
		"SELECT id, user_id, token_hash, expires_at FROM refresh_tokens WHERE expires_at > NOW()",
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, userID int64
		var hash string
		var exp time.Time
		if err := rows.Scan(&id, &userID, &hash, &exp); err != nil {
			return 0, err
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(raw)) == nil {
			if _, err := s.DB.Exec("DELETE FROM refresh_tokens WHERE id = $1", id); err != nil {
				return 0, err
			}
			return userID, nil
		}
	}
	return 0, errors.New("invalid refresh token")
}

// Middleware wraps an HTTP handler and rejects requests that don't carry
// a valid Bearer access token.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			w.Header().Set("Content-Type", "application/json")
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
