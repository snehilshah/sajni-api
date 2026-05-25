package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Google implements OAuth 2.0 + OpenID Connect against accounts.google.com.
// Scopes are kept minimal: openid email profile. We never request the
// avatar/photo URL.
type Google struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

func (g *Google) Name() string { return "google" }

func (g *Google) StartURL(state string) string {
	q := url.Values{}
	q.Set("client_id", g.ClientID)
	q.Set("redirect_uri", g.RedirectURI)
	q.Set("response_type", "code")
	// `openid email` only — the `profile` scope is what makes Google's
	// consent screen prompt for "view your profile picture", which we
	// don't want. We derive a display name from the email local-part.
	q.Set("scope", "openid email")
	q.Set("state", state)
	q.Set("access_type", "online")
	q.Set("prompt", "select_account")
	return "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode()
}

func (g *Google) Exchange(ctx context.Context, code string) (*Identity, error) {
	form := url.Values{}
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", g.RedirectURI)

	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://oauth2.googleapis.com/token",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("google token exchange: %s", string(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("google token decode: %w", err)
	}

	// Use the userinfo endpoint — simpler than parsing the JWT and we
	// already trust the response over TLS to googleapis.com.
	req2, _ := http.NewRequestWithContext(ctx, "GET",
		"https://openidconnect.googleapis.com/v1/userinfo", nil)
	req2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	r2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("google userinfo: %w", err)
	}
	defer r2.Body.Close()
	b2, _ := io.ReadAll(r2.Body)
	if r2.StatusCode != 200 {
		return nil, fmt.Errorf("google userinfo: %s", string(b2))
	}
	// Without the `profile` scope the userinfo response only carries
	// sub + email + email_verified — no name/picture fields. We fall
	// back to the email local-part for a display name.
	var info struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(b2, &info); err != nil {
		return nil, fmt.Errorf("google userinfo decode: %w", err)
	}
	email := strings.ToLower(strings.TrimSpace(info.Email))
	name := email
	if at := strings.Index(email, "@"); at > 0 {
		name = email[:at]
	}
	return &Identity{
		Subject:       info.Sub,
		Email:         email,
		EmailVerified: info.EmailVerified,
		Name:          name,
	}, nil
}
