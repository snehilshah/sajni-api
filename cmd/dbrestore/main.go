// Command dbrestore loads a database backup (internal/backup) into a fresh
// database. Manual only; it never reads .env, so it can't hit prod by
// accident: the target URL is always given on the command line.
//
//	go run ./cmd/dbrestore -from ./restore -db 'postgres://…'
//
// The target must already hold the current schema (start the API against it
// once). Every table is emptied and reloaded in one transaction; a target
// that already has users is refused unless -force.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"sajni/internal/backup"
)

func main() {
	from := flag.String("from", "", "folder holding <table>.csv.gz (or .csv) files")
	dsn := flag.String("db", "", "target database URL (holds the current schema)")
	force := flag.Bool("force", false, "replace a target that already has users")
	flag.Parse()
	if *from == "" || *dsn == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*from, *dsn, *force); err != nil {
		fmt.Fprintln(os.Stderr, "restore:", err)
		os.Exit(1)
	}
}

func run(from, dsn string, force bool) error {
	ctx := context.Background()
	sdb, err := openDB(dsn)
	if err != nil {
		return err
	}
	defer sdb.Close()
	var users int
	if err := sdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return fmt.Errorf("target has no Sajni schema (start the API against it once): %w", err)
	}
	if users > 0 && !force {
		return fmt.Errorf("target already has %d users; pass -force to replace everything in it", users)
	}
	isMissing := func(err error) bool { return errors.Is(err, fs.ErrNotExist) }
	open := backup.GzipOrPlain(func(name string) (io.ReadCloser, error) {
		return os.Open(filepath.Join(from, name))
	}, isMissing)
	counts, err := backup.Restore(ctx, sdb, open, isMissing)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(counts))
	var total int64
	for t, n := range counts {
		names = append(names, t)
		total += n
	}
	sort.Strings(names)
	for _, t := range names {
		fmt.Printf("%-32s %8d\n", t, counts[t])
	}
	fmt.Printf("\nRestored %d tables, %d rows. Compare with RESTORE.md.\n", len(counts), total)
	return nil
}

func openDB(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*cfg), nil
}
