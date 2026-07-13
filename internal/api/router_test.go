package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCorsLocalPrivateOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{origin: "http://localhost:5173", want: true},
		{origin: "http://192.168.1.22:5173", want: true},
		{origin: "http://10.0.0.12:5173", want: true},
		{origin: "http://sajni-dev.local:5173", want: true},
		{origin: "https://www.ohmysajni.com", want: false},
		{origin: "https://evil.example", want: false},
		{origin: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.origin, func(t *testing.T) {
			if got := corsLocalPrivateOrigin(tt.origin); got != tt.want {
				t.Fatalf("corsLocalPrivateOrigin(%q) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}

func TestCORSOriginPolicy(t *testing.T) {
	tests := []struct {
		name       string
		allowed    string
		allowLocal string
		origin     string
		wantOrigin string
	}{
		{name: "configured origin", allowed: "https://www.ohmysajni.com", origin: "https://www.ohmysajni.com", wantOrigin: "https://www.ohmysajni.com"},
		{name: "unlisted origin", allowed: "https://www.ohmysajni.com", origin: "https://evil.example"},
		{name: "missing config fails closed", origin: "https://evil.example"},
		{name: "explicit local development", allowLocal: "1", origin: "http://192.168.1.22:5173", wantOrigin: "http://192.168.1.22:5173"},
		{name: "local mode rejects public origin", allowLocal: "1", origin: "https://evil.example"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CORS_ORIGIN", tt.allowed)
			t.Setenv("ALLOW_LOCAL_CORS", tt.allowLocal)
			h := withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Origin", tt.origin)
			res := httptest.NewRecorder()

			h.ServeHTTP(res, req)

			if got := res.Header().Get("Access-Control-Allow-Origin"); got != tt.wantOrigin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantOrigin)
			}
		})
	}
}

func TestErrJSONHidesInternalDetails(t *testing.T) {
	res := httptest.NewRecorder()
	errJSON(res, http.StatusInternalServerError, errors.New("private database detail").Error())

	if got := res.Body.String(); got != "{\"error\":\"internal error\"}\n" {
		t.Fatalf("body = %q", got)
	}
}
