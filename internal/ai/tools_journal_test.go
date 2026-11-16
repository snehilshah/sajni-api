package ai

import (
	"context"
	"os"
	"testing"

	"sajni/internal/db"
	"sajni/internal/storage"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/ai
func TestCreateJournalAppends(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	uid := "01900000-0000-7000-8000-0000000000f7"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'journaltool@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	write := func(args map[string]any) {
		t.Helper()
		args["date"] = "2026-11-16"
		if _, _, err := createJournalTool(ctx, d, store, uid, args); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		b, _, err := store.Get(ctx, storage.UserKey(uid, "journal", "2026-11-16.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	write(map[string]any{"content": "Morning run, 5 km.", "location_label": "Vashi"})
	write(map[string]any{"content": "Lunch with Riya #friends [[Reading list]]"})
	if got, want := read(), "Morning run, 5 km.\n\nLunch with Riya #friends [[Reading list]]"; got != want {
		t.Fatalf("append: got %q, want %q", got, want)
	}
	var loc string
	var tags, links int
	d.QueryRow(`SELECT location_label FROM journal_entries WHERE user_id=$1`, uid).Scan(&loc)
	d.QueryRow(`SELECT COUNT(*) FROM tags WHERE user_id=$1 AND entity_type='journal'`, uid).Scan(&tags)
	d.QueryRow(`SELECT COUNT(*) FROM backlinks WHERE user_id=$1 AND source_type='journal'`, uid).Scan(&links)
	if loc != "Vashi" || tags != 1 || links != 1 {
		t.Fatalf("location %q (want kept), tags %d, links %d (want 1, 1)", loc, tags, links)
	}

	write(map[string]any{"content": "Rewritten.", "mode": "replace"})
	if got := read(); got != "Rewritten." {
		t.Fatalf("replace: got %q", got)
	}
	d.QueryRow(`SELECT COUNT(*) FROM tags WHERE user_id=$1 AND entity_type='journal'`, uid).Scan(&tags)
	if tags != 0 {
		t.Fatalf("tags after replace: %d", tags)
	}
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
}
