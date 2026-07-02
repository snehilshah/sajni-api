package auth

import (
	"net/http/httptest"
	"testing"
)

func TestDevAuthBypassAllowed(t *testing.T) {
	tests := []struct {
		name   string
		on     bool
		host   string
		origin string
		want   bool
	}{
		{name: "disabled denies local", on: false, host: "localhost:8080", origin: "http://localhost:5173", want: false},
		{name: "localhost origin allowed", on: true, host: "localhost:8080", origin: "http://localhost:5173", want: true},
		{name: "private lan origin allowed", on: true, host: "localhost:8080", origin: "http://192.168.1.22:5173", want: true},
		{name: "public origin denied", on: true, host: "api.ohmysajni.com", origin: "https://evil.example", want: false},
		{name: "public host without origin denied", on: true, host: "api.ohmysajni.com", origin: "", want: false},
		{name: "private api host allowed without origin", on: true, host: "10.0.0.12:8080", origin: "", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "http://"+tt.host+"/api/auth/refresh", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			s := &Service{DevAuthBypass: tt.on}
			if got := s.devAuthBypassAllowed(req); got != tt.want {
				t.Fatalf("devAuthBypassAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}
