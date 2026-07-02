package auth

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

func (s *Service) devAuthBypassAllowed(r *http.Request) bool {
	if !s.DevAuthBypass {
		return false
	}
	for _, raw := range []string{
		r.Header.Get("Origin"),
		r.Header.Get("Referer"),
		"http://" + r.Host,
		"https://" + r.Host,
	} {
		if localPrivateURL(raw) {
			return true
		}
	}
	return false
}

func localPrivateURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return localPrivateHost(u.Host)
}

func localPrivateHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	switch {
	case host == "localhost", host == "0.0.0.0", host == "::1":
		return true
	case strings.HasSuffix(host, ".localhost"), strings.HasSuffix(host, ".local"):
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback() || addr.IsPrivate()
}

func (s *Service) issueDevBypassSession(ctx context.Context, w http.ResponseWriter) (*authResponse, error) {
	userID, err := s.ensureDevBypassUser(ctx)
	if err != nil {
		return nil, err
	}
	return s.issueSession(ctx, w, userID)
}

func (s *Service) ensureDevBypassUser(ctx context.Context) (string, error) {
	email := strings.ToLower(strings.TrimSpace(s.DevAuthEmail))
	if email == "" || !strings.Contains(email, "@") {
		return "", errors.New("DEV_AUTH_BYPASS_EMAIL must be a valid email")
	}
	name := strings.TrimSpace(s.DevAuthName)
	if name == "" {
		name = "Sajni Dev"
	}

	var userID string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE email=$1`, email).Scan(&userID)
	switch {
	case err == nil:
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE users
			    SET name = CASE WHEN COALESCE(name,'') = '' THEN $2 ELSE name END,
			        onboarded_at = COALESCE(onboarded_at, NOW()),
			        deleted_at = NULL
			  WHERE id = $1`,
			userID, name,
		); err != nil {
			return "", err
		}
	case errors.Is(err, sql.ErrNoRows):
		userID = NewID()
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO users (id, email, name, onboarded_at) VALUES ($1, $2, $3, NOW())`,
			userID, email, name,
		); err != nil {
			return "", err
		}
	default:
		return "", err
	}

	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO auth_identities (id, user_id, provider, provider_subject, email, last_used_at)
		 VALUES ($1, $2, 'email', $3, $4, NOW())
		 ON CONFLICT (provider, provider_subject)
		 DO UPDATE SET user_id = EXCLUDED.user_id, email = EXCLUDED.email, last_used_at = NOW()`,
		NewID(), userID, email, email,
	); err != nil {
		return "", err
	}
	return userID, nil
}
