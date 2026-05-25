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

// GitHub implements OAuth 2.0 against github.com. Scopes: read:user
// user:email so we can pick the primary verified email even when the
// user has hidden it from their public profile.
type GitHub struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

func (g *GitHub) Name() string { return "github" }

func (g *GitHub) StartURL(state string) string {
	q := url.Values{}
	q.Set("client_id", g.ClientID)
	q.Set("redirect_uri", g.RedirectURI)
	q.Set("scope", "read:user user:email")
	q.Set("state", state)
	q.Set("allow_signup", "true")
	return "https://github.com/login/oauth/authorize?" + q.Encode()
}

func (g *GitHub) Exchange(ctx context.Context, code string) (*Identity, error) {
	form := url.Values{}
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", g.RedirectURI)

	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://github.com/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github token exchange: %s", string(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("github token decode: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("github token exchange failed: %s", tok.Error)
	}

	// /user has id + name; /user/emails has the verified primary.
	userReq, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user", nil)
	userReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	userReq.Header.Set("Accept", "application/vnd.github+json")
	ru, err := http.DefaultClient.Do(userReq)
	if err != nil {
		return nil, fmt.Errorf("github user: %w", err)
	}
	defer ru.Body.Close()
	ub, _ := io.ReadAll(ru.Body)
	if ru.StatusCode != 200 {
		return nil, fmt.Errorf("github user: %s", string(ub))
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(ub, &user); err != nil {
		return nil, fmt.Errorf("github user decode: %w", err)
	}

	emailsReq, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user/emails", nil)
	emailsReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	emailsReq.Header.Set("Accept", "application/vnd.github+json")
	re, err := http.DefaultClient.Do(emailsReq)
	if err != nil {
		return nil, fmt.Errorf("github emails: %w", err)
	}
	defer re.Body.Close()
	eb, _ := io.ReadAll(re.Body)
	if re.StatusCode != 200 {
		return nil, fmt.Errorf("github emails: %s", string(eb))
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	json.Unmarshal(eb, &emails)

	email := user.Email
	verified := false
	for _, e := range emails {
		if e.Primary {
			email = e.Email
			verified = e.Verified
			break
		}
	}
	if email == "" && len(emails) > 0 {
		// No primary returned — fall back to first verified.
		for _, e := range emails {
			if e.Verified {
				email = e.Email
				verified = true
				break
			}
		}
	}

	name := user.Name
	if name == "" {
		name = user.Login
	}

	return &Identity{
		Subject:       fmt.Sprintf("%d", user.ID),
		Email:         strings.ToLower(strings.TrimSpace(email)),
		EmailVerified: verified,
		Name:          name,
	}, nil
}
