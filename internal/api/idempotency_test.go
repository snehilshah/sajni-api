package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"sajni/internal/auth"
	"sajni/internal/db"
)

// A queued change replayed with the same key runs once and gets the first
// reply back; a failed one (5xx) runs again.
// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run Idempotency
func TestIdempotency(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-00000000000d"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'idem@test', 'T')`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)

	runs, fail := 0, false
	h := withIdempotency(d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs++
		if fail {
			errJSON(w, 503, "down")
			return
		}
		writeJSON(w, 201, map[string]int{"id": 40 + runs})
	}))
	call := func(key string) (int, string, http.Header) {
		req := httptest.NewRequest("POST", "/api/memos", bytes.NewBufferString(`{}`))
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		if key != "" {
			req.Header.Set(IdempotencyHeader, key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String(), rec.Header()
	}

	code, body, _ := call("k1")
	if code != 201 || body != "{\"id\":41}\n" {
		t.Fatalf("first: %d %q", code, body)
	}
	code, body, hdr := call("k1")
	if code != 201 || body != "{\"id\":41}\n" || runs != 1 || hdr.Get("X-Idempotency-Replayed") != "1" {
		t.Fatalf("replay: %d %q runs=%d", code, body, runs)
	}
	// No key: runs every time.
	call("")
	call("")
	if runs != 3 {
		t.Fatalf("unkeyed runs: %d", runs)
	}
	// A 5xx isn't kept, so the retry runs.
	fail = true
	if code, _, _ := call("k2"); code != 503 {
		t.Fatalf("fail: %d", code)
	}
	fail = false
	if code, body, _ := call("k2"); code != 201 || body != "{\"id\":45}\n" {
		t.Fatalf("retry after 5xx: %d %q", code, body)
	}
	// A key still running asks the client to come back.
	d.Exec(`INSERT INTO idempotency_keys (user_id, key) VALUES ($1, 'k3')`, uid)
	if code, _, hdr := call("k3"); code != 409 || hdr.Get("X-Idempotency-Pending") != "1" {
		t.Fatalf("pending: %d", code)
	}
	// One left running for minutes is taken over.
	d.Exec(`UPDATE idempotency_keys SET created_at = NOW() - INTERVAL '3 minutes' WHERE user_id = $1 AND key = 'k3'`, uid)
	if code, _, _ := call("k3"); code != 201 {
		t.Fatalf("stale claim: %d", code)
	}
}
