package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"sajni/internal/db"
	"sajni/internal/storage"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/backup
func TestWeeklyBackup(t *testing.T) {
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
	d.Exec(`DELETE FROM db_backups`)

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
	if next, _ := Due(ctx, d, at.Add(6*24*time.Hour)); next != 0 {
		t.Fatal("claimed again within a week")
	}
	if next, _ := Due(ctx, d, at.Add(7*24*time.Hour)); next == 0 {
		t.Fatal("not claimed a week later")
	}
	d.Exec(`DELETE FROM db_backups`)
}
