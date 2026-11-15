package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithNoStore(t *testing.T) {
	plain := withNoStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	rec := httptest.NewRecorder()
	plain.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("default Cache-Control = %q", got)
	}

	own := withNoStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Write([]byte("img"))
	}))
	rec = httptest.NewRecorder()
	own.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/uploads/a.jpg", nil))
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=3600" {
		t.Fatalf("handler's own Cache-Control was overridden: %q", got)
	}
}
