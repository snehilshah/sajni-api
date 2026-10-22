package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The media cookie opens uploads to a plain <img> and nothing else.
func TestMediaCookieOnlyReadsUploads(t *testing.T) {
	s := &Service{JWTSecret: []byte("test-secret")}
	const uid = "7f1c1a52-5a0c-4c55-9a52-0d5b8c1b2e11"
	media, err := s.issueMediaToken(uid)
	if err != nil {
		t.Fatal(err)
	}
	access, err := s.issueAccessToken(uid)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, _ := UserIDFromContext(r.Context()); got != uid {
			t.Errorf("user = %q", got)
		}
	}))
	cases := []struct {
		name, method, path, cookie, bearer string
		want                               int
	}{
		{"cookie reads an upload", http.MethodGet, "/api/uploads/a.png", media, "", 200},
		{"cookie can't read other routes", http.MethodGet, "/api/tasks", media, "", 401},
		{"cookie can't upload", http.MethodPost, "/api/uploads", media, "", 401},
		{"media token isn't a bearer", http.MethodGet, "/api/tasks", "", media, 401},
		{"access token isn't a media cookie", http.MethodGet, "/api/uploads/a.png", access, "", 401},
		{"bearer still reads uploads", http.MethodGet, "/api/uploads/a.png", "", access, 200},
		{"no credentials", http.MethodGet, "/api/uploads/a.png", "", "", 401},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if c.cookie != "" {
			r.AddCookie(&http.Cookie{Name: mediaCookie, Value: c.cookie})
		}
		if c.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+c.bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.want)
		}
	}
}
