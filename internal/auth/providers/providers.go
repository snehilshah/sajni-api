// Package providers wraps the Google + GitHub OAuth 2.0 flows that
// power Sajni sign-in. Each provider exposes the same shape:
//
//	StartURL(state) — the consent URL to redirect the browser to
//	Exchange(ctx, code) — turn the callback `code` into a normalized
//	  Identity { Subject, Email, EmailVerified, Name }.
//
// The auth service composes these with the linking algorithm in
// internal/auth/linking.go.
package providers

import "context"

// Identity is the normalized payload an OAuth provider returns about
// the user who just signed in.
type Identity struct {
	Subject       string // provider-stable id (Google sub, GitHub id)
	Email         string
	EmailVerified bool
	Name          string
}

// Provider is the minimal surface every OAuth provider exposes.
type Provider interface {
	Name() string
	StartURL(state string) string
	Exchange(ctx context.Context, code string) (*Identity, error)
}
