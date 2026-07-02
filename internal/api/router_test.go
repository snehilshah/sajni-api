package api

import "testing"

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
