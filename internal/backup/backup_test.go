package backup

import (
	"database/sql"
	"errors"
	"net/url"

	"bytes"
	"compress/gzip"
	"context"
	_ "github.com/jackc/pgx/v5/stdlib"
	"io"
	"sajni/internal/dbtest"
	"strings"
	"testing"
	"time"

	"sajni/internal/db"
	"sajni/internal/storage"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/backup
func TestWeeklyBackup(t *testing.T) {
	dsn := dbtest.URL(t)
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
	d.Exec(`DELETE FROM db_backups`)
	// Something in a serial table and a child row, so order and sequences matter.
	uid := "019e0000-0000-7000-8000-00000000b4c0"
	d.Exec(`DELETE FROM users WHERE id=$1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'backup@test', 'B')`, uid); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id=$1`, uid)
	if _, err := d.Exec(`INSERT INTO tasks (user_id, title, description) VALUES ($1, 'Pack, "quoted"', E'two\nlines')`, uid); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 11, 15, Hour, 5, 0, 0, time.UTC)
	if id, _ := Due(ctx, d, at.Add(-time.Hour)); id != 0 {
		t.Fatal("claimed outside the backup hour")
	}
	id, err := Due(ctx, d, at)
	if err != nil || id == 0 {
		t.Fatalf("not claimed: %v", err)
	}
	if again, _ := Due(ctx, d, at.Add(10*time.Minute)); again != 0 {
		t.Fatal("claimed twice while one is running")
	}
	res, err := Run(ctx, d, store, id, at)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tables < 50 {
		t.Fatalf("only %d tables", res.Tables)
	}
	raw, _, err := store.Get(ctx, res.Prefix+"users.csv.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	csv, _ := io.ReadAll(zr)
	if !strings.HasPrefix(string(csv), "id,") {
		t.Fatalf("users.csv starts %q", string(csv[:min(len(csv), 40)]))
	}
	if _, _, err := store.Get(ctx, res.Prefix+"RESTORE.md"); err != nil {
		t.Fatal(err)
	}
	restoreRoundTrip(t, dsn, store, res)
	if next, _ := Due(ctx, d, at.Add(6*24*time.Hour)); next != 0 {
		t.Fatal("claimed again within a week")
	}
	if next, _ := Due(ctx, d, at.Add(7*24*time.Hour)); next == 0 {
		t.Fatal("not claimed a week later")
	}
	d.Exec(`DELETE FROM db_backups`)
}

// restoreRoundTrip loads the backup into a second scratch database and
// checks every table comes back with the same rows, and that the sequences
// moved past them.
func restoreRoundTrip(t *testing.T, dsn string, store storage.Storage, res Result) {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const name = "sajni_test_restore"
	admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Skipf("can't create a second database: %v", err)
	}
	defer admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	u, _ := url.Parse(dsn)
	u.Path = "/" + name
	if why := dbtest.Check(u.String()); why != "" {
		t.Fatal(why)
	}
	target, err := db.New(u.String()) // creates the current schema
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	ctx := context.Background()
	isMissing := func(err error) bool { return errors.Is(err, storage.ErrNotFound) }
	open := GzipOrPlain(func(name string) (io.ReadCloser, error) {
		raw, _, err := store.Get(ctx, res.Prefix+name)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(raw)), nil
	}, isMissing)
	got, err := Restore(ctx, target.DB, open, isMissing)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range res.Counts {
		if got[table] != want {
			t.Errorf("%s: restored %d rows, backup had %d", table, got[table], want)
		}
	}
	var title, desc string
	if err := target.QueryRow(`SELECT title, description FROM tasks WHERE title LIKE 'Pack%'`).Scan(&title, &desc); err != nil || desc != "two\nlines" {
		t.Fatalf("task came back as %q %q: %v", title, desc, err)
	}
	// A new row must not collide with a restored id.
	var uid string
	target.QueryRow(`SELECT user_id FROM tasks LIMIT 1`).Scan(&uid)
	if _, err := target.Exec(`INSERT INTO tasks (user_id, title) VALUES ($1, 'after restore')`, uid); err != nil {
		t.Fatalf("insert after restore: %v", err)
	}
}
