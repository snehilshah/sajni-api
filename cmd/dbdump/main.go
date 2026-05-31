// Command dbdump makes a restorable, on-demand backup of the Postgres
// database WITHOUT any external client tooling (no pg_dump/psql needed).
//
// For every base table in the public schema it streams a server-native CSV
// (COPY ... TO STDOUT WITH (FORMAT csv, HEADER true)) to one .csv file in a
// timestamped folder, then writes a RESTORE.md manifest with row counts and
// the exact restore recipe.
//
//	DO NOT RUN automatically / in CI. Manual, on-demand only.
//
// Usage (reads DATABASE_URL from .env, like the server):
//
//	go run ./cmd/dbdump [out_dir]      # default out_dir = ./backups
//
// Restore (deliberate, manual): start the server once so migrations recreate
// the empty schema, then for each table — in any order, FK checks deferred:
//
//	BEGIN;
//	SET session_replication_role = replica;   -- skip FK/trigger checks
//	\copy "<table>" FROM '<table>.csv' WITH (FORMAT csv, HEADER true)
//	... (repeat for every table) ...
//	SET session_replication_role = origin;
//	COMMIT;
//
// CSV here is the server's own COPY format, so it round-trips NULLs and bytea
// exactly via COPY ... FROM (FORMAT csv).
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// loadDotEnv mirrors cmd/main.go: KEY=VALUE lines, no override of real env.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
}

func main() {
	loadDotEnv(".env")

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required (set it or put it in .env)")
		os.Exit(1)
	}

	outRoot := "./backups"
	if len(os.Args) > 1 {
		outRoot = os.Args[1]
	}
	stamp := time.Now().Format("20060102-150405")
	outDir := filepath.Join(outRoot, "csv-"+stamp)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", outDir, err)
		os.Exit(1)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	// Every base table in the public schema.
	rows, err := conn.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list tables: %v\n", err)
		os.Exit(1)
	}
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			fmt.Fprintf(os.Stderr, "scan table: %v\n", err)
			os.Exit(1)
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "iterate tables: %v\n", err)
		os.Exit(1)
	}
	sort.Strings(tables)

	counts := make(map[string]int64, len(tables))
	var grandRows int64
	for _, t := range tables {
		path := filepath.Join(outDir, t+".csv")
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", path, err)
			os.Exit(1)
		}
		// Quote the identifier to be safe; tables are server-known names.
		sql := fmt.Sprintf(`COPY (SELECT * FROM %q) TO STDOUT WITH (FORMAT csv, HEADER true)`, t)
		tag, err := conn.PgConn().CopyTo(ctx, f, sql)
		f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "copy %s: %v\n", t, err)
			os.Exit(1)
		}
		n := tag.RowsAffected()
		counts[t] = n
		grandRows += n
		fmt.Printf("-> %-28s %8d rows\n", t, n)
	}

	writeManifest(outDir, stamp, dsn, tables, counts, grandRows)
	fmt.Printf("\nDone. %d tables, %d rows -> %s\n", len(tables), grandRows, outDir)
}

func writeManifest(outDir, stamp, dsn string, tables []string, counts map[string]int64, grand int64) {
	host := dsn
	if i := strings.Index(dsn, "@"); i >= 0 {
		host = dsn[i+1:] // strip credentials
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Sajni DB backup — %s\n\n", stamp)
	fmt.Fprintf(&b, "Source host: `%s`\n\n", host)
	fmt.Fprintf(&b, "%d tables, %d rows total.\n\n", len(tables), grand)
	b.WriteString("## Tables\n\n| table | rows |\n| --- | ---: |\n")
	for _, t := range tables {
		fmt.Fprintf(&b, "| %s | %d |\n", t, counts[t])
	}
	b.WriteString("\n## Restore\n\n")
	b.WriteString("1. Start the API once against the target DB so migrations recreate the empty schema.\n")
	b.WriteString("2. Then, with FK/trigger checks deferred, COPY each CSV back:\n\n")
	b.WriteString("```sql\nBEGIN;\nSET session_replication_role = replica;\n")
	for _, t := range tables {
		fmt.Fprintf(&b, "\\copy \"%s\" FROM '%s.csv' WITH (FORMAT csv, HEADER true)\n", t, t)
	}
	b.WriteString("SET session_replication_role = origin;\nCOMMIT;\n```\n")
	b.WriteString("\nCSV is Postgres' native COPY format, so NULLs and bytea round-trip exactly.\n")
	_ = os.WriteFile(filepath.Join(outDir, "RESTORE.md"), []byte(b.String()), 0o644)
}
