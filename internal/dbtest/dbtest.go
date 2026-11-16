// Package dbtest guards the database tests. They insert and delete freely,
// so they run only against a scratch Postgres: a local host and a database
// whose name contains "test" (AGENTS.md → Dev & verification). This keeps a
// stray SAJNI_TEST_DATABASE_URL (say, copied from the prod .env) from ever
// touching real data.
package dbtest

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

// URL returns SAJNI_TEST_DATABASE_URL, skips the test when it's unset, and
// fails it when the URL doesn't look like a scratch database.
func URL(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	if err := Check(dsn); err != "" {
		t.Fatalf("SAJNI_TEST_DATABASE_URL refused: %s", err)
	}
	return dsn
}

// Check explains why dsn isn't a scratch database, or returns "".
func Check(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "not a postgres:// URL"
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
	default:
		return "host " + u.Hostname() + " is not local"
	}
	if !strings.Contains(strings.ToLower(strings.TrimPrefix(u.Path, "/")), "test") {
		return "database name has no \"test\" in it"
	}
	return ""
}
