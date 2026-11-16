// Command dbdump makes an on-demand local copy of the Postgres database, in
// the same form as the weekly backup (internal/backup): one <table>.csv.gz
// per table from a single snapshot, plus RESTORE.md. Restore it with
// cmd/dbrestore. Manual only, never in CI.
//
//	go run ./cmd/dbdump [out_dir]      # reads DATABASE_URL from .env; default ./backups
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"sajni/internal/backup"
	"sajni/internal/config"
)

func main() {
	databaseConfig, err := config.LoadDatabase(".env")
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration: %v\n", err)
		os.Exit(1)
	}
	outRoot := "./backups"
	if len(os.Args) > 1 {
		outRoot = os.Args[1]
	}
	now := time.Now()
	outDir := filepath.Join(outRoot, "csv-"+now.Format("20060102-150405"))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", outDir, err)
		os.Exit(1)
	}
	cfg, err := pgx.ParseConfig(databaseConfig.URL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database url: %v\n", err)
		os.Exit(1)
	}
	sdb := stdlib.OpenDB(*cfg)
	defer sdb.Close()
	res, err := backup.Dump(context.Background(), sdb, now, func(name string, data []byte, _ string) error {
		return os.WriteFile(filepath.Join(outDir, name), data, 0o600)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dump: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Done. %d tables, %d rows -> %s\n", res.Tables, res.Rows, outDir)
}
