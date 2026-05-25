package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOAuthBaseForRequestUsesConfiguredAppOriginFromProxy(t *testing.T) {
	s := &Service{
		AppURL:  "https://ohmysajni.com",
		APIBase: "https://sajni-api-a7bzmoi2qq-el.a.run.app",
	}
	r := httptest.NewRequest(http.MethodGet, "https://sajni-api-a7bzmoi2qq-el.a.run.app/api/auth/google/start", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "ohmysajni.com")

	if got, want := s.oauthBaseForRequest(r), "https://ohmysajni.com"; got != want {
		t.Fatalf("oauthBaseForRequest() = %q, want %q", got, want)
	}
}

func TestOAuthBaseForRequestRejectsUnknownForwardedHost(t *testing.T) {
	s := &Service{
		AppURL:  "https://ohmysajni.com",
		APIBase: "https://sajni-api-a7bzmoi2qq-el.a.run.app",
	}
	r := httptest.NewRequest(http.MethodGet, "https://sajni-api-a7bzmoi2qq-el.a.run.app/api/auth/google/start", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "evil.example")

	if got, want := s.oauthBaseForRequest(r), "https://sajni-api-a7bzmoi2qq-el.a.run.app"; got != want {
		t.Fatalf("oauthBaseForRequest() = %q, want %q", got, want)
	}
}

func TestOAuthBaseForRequestSupportsForwardedHeader(t *testing.T) {
	s := &Service{
		AppURL:  "https://www.ohmysajni.com",
		APIBase: "https://sajni-api-a7bzmoi2qq-el.a.run.app",
	}
	r := httptest.NewRequest(http.MethodGet, "https://sajni-api-a7bzmoi2qq-el.a.run.app/api/auth/github/start", nil)
	r.Header.Set("Forwarded", `for=203.0.113.1; proto=https; host="www.ohmysajni.com"`)

	if got, want := s.oauthBaseForRequest(r), "https://www.ohmysajni.com"; got != want {
		t.Fatalf("oauthBaseForRequest() = %q, want %q", got, want)
	}
}

func TestOAuthStateCarriesAllowedRedirectBase(t *testing.T) {
	s := &Service{
		JWTSecret: []byte("test-secret"),
		AppURL:    "https://ohmysajni.com",
		APIBase:   "https://sajni-api-a7bzmoi2qq-el.a.run.app",
	}
	state, err := s.makeOAuthState("https://ohmysajni.com")
	if err != nil {
		t.Fatalf("makeOAuthState() error = %v", err)
	}

	got, err := s.verifyOAuthState(state)
	if err != nil {
		t.Fatalf("verifyOAuthState() error = %v", err)
	}
	if want := "https://ohmysajni.com"; got != want {
		t.Fatalf("verifyOAuthState() base = %q, want %q", got, want)
	}
}
