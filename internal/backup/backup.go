// Package backup writes a weekly copy of the whole Postgres database to the
// blob bucket, so the "no data loss" rule has a backstop (the free database
// tier keeps no backups of its own), and restores one.
//
// Each run writes backups/db/<UTC stamp>/<table>.csv.gz, one per public base
// table in Postgres' own COPY CSV format (NULLs and bytea round-trip; the
// header row names the columns), plus RESTORE.md with row counts and how to
// restore: `go run ./cmd/dbrestore`, which loads every file into a fresh
// schema in foreign-key order, by column name, and resets the sequences
// (Restore). A bucket lifecycle rule on the backups/ prefix deletes old runs
// (sajni-api/DEPLOY.md → Backups).
package backup

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/rs/zerolog/log"

	"sajni/internal/db"
	"sajni/internal/storage"
)

// Every is how often a backup runs; Hour is the UTC hour it may start in
// (21:00 UTC = 02:30 IST, the quietest time). Deadline bounds one run, under
// the scheduler's request timeout.
const (
	Every    = 7 * 24 * time.Hour
	Hour     = 21
	Deadline = 45 * time.Second
)

// Result says what one run wrote.
type Result struct {
	Prefix string
	Tables int
	Rows   int64
	Bytes  int
	Counts map[string]int64
	Took   time.Duration
}

// Due claims a backup when the last finished one is older than Every and
// none started in the last two hours (a run in flight, or one that failed
// recently). It returns the claimed row's id, or 0 when nothing is due.
func Due(ctx context.Context, d *db.DB, now time.Time) (int64, error) {
	if now.UTC().Hour() != Hour {
		return 0, nil
	}
	var id int64
	err := d.QueryRowContext(ctx, `
		INSERT INTO db_backups (started_at)
		SELECT $1::timestamptz WHERE NOT EXISTS (
			SELECT 1 FROM db_backups
			WHERE (finished_at IS NOT NULL AND finished_at > $1::timestamptz - make_interval(secs => $2))
			   OR started_at > $1::timestamptz - interval '2 hours')
		RETURNING id`, now, int((Every - 2*time.Hour).Seconds())).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// Run dumps every table to the store and records the result on the claimed
// row. It stops after Deadline; the outcome is recorded even then.
func Run(ctx context.Context, d *db.DB, store storage.Storage, id int64, now time.Time) (Result, error) {
	began := time.Now()
	rctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	prefix := "backups/db/" + now.UTC().Format("20060102-150405") + "/"
	res, err := Dump(rctx, d.DB, now, func(name string, data []byte, contentType string) error {
		return store.Put(rctx, prefix+name, data, contentType)
	})
	res.Prefix = prefix
	res.Took = time.Since(began)
	// The bookkeeping must land even when the run was cut short.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer bcancel()
	if err != nil {
		_, _ = d.ExecContext(bctx, `UPDATE db_backups SET error=$2 WHERE id=$1`, id, err.Error())
		return res, err
	}
	_, err = d.ExecContext(bctx, `UPDATE db_backups SET finished_at=$6, prefix=$2, tables=$3, row_count=$4, bytes=$5, error='' WHERE id=$1`,
		id, res.Prefix, res.Tables, res.Rows, res.Bytes, now.Add(res.Took))
	return res, err
}

// Put stores one file of a backup.
type Put func(name string, data []byte, contentType string) error

// Dump writes <table>.csv.gz for every public base table, all from one
// snapshot, then RESTORE.md. cmd/dbdump uses it for a local copy.
func Dump(ctx context.Context, sdb *sql.DB, now time.Time, put Put) (Result, error) {
	res := Result{Counts: map[string]int64{}}
	conn, err := sdb.Conn(ctx)
	if err != nil {
		return res, err
	}
	defer conn.Close()

	err = conn.Raw(func(dc any) error {
		sc, ok := dc.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("backup needs the pgx driver, got %T", dc)
		}
		pc := sc.Conn()
		// One repeatable-read snapshot, so every table is from the same
		// moment. pgx's Tx always ends (or closes the connection), so the
		// pooled connection never goes back mid-transaction.
		tx, err := pc.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // read-only

		tables, err := listTables(ctx, tx)
		if err != nil {
			return err
		}
		for _, t := range tables {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			tag, err := tx.Conn().PgConn().CopyTo(ctx, zw, `COPY `+pgx.Identifier{t}.Sanitize()+` TO STDOUT WITH (FORMAT csv, HEADER true)`)
			if err == nil {
				err = zw.Close()
			}
			if err != nil {
				return fmt.Errorf("copy %s: %w", t, err)
			}
			if err := put(t+".csv.gz", buf.Bytes(), "application/gzip"); err != nil {
				return fmt.Errorf("store %s: %w", t, err)
			}
			res.Counts[t] = tag.RowsAffected()
			res.Rows += tag.RowsAffected()
			res.Bytes += buf.Len()
		}
		res.Tables = len(tables)
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, put("RESTORE.md", []byte(restoreGuide(now, res)), "text/markdown")
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// listTables returns the public base tables, parents before the tables whose
// foreign keys point at them (ties by name), so loading in this order never
// trips a key check. A foreign-key cycle between tables is an error.
func listTables(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public'`)
	if err != nil {
		return nil, err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `
		SELECT DISTINCT c.relname, p.relname
		FROM pg_constraint k
		JOIN pg_class c ON c.oid=k.conrelid
		JOIN pg_class p ON p.oid=k.confrelid
		WHERE k.contype='f' AND k.connamespace='public'::regnamespace AND k.conrelid<>k.confrelid`)
	if err != nil {
		return nil, err
	}
	type edge struct{ Child, Parent string }
	edges, err := pgx.CollectRows(rows, pgx.RowToStructByPos[edge])
	if err != nil {
		return nil, err
	}
	parents := map[string]map[string]bool{}
	for _, e := range edges {
		if parents[e.Child] == nil {
			parents[e.Child] = map[string]bool{}
		}
		parents[e.Child][e.Parent] = true
	}
	sort.Strings(tables)
	done := map[string]bool{}
	var order []string
	for len(order) < len(tables) {
		progressed := false
		for _, t := range tables {
			if done[t] {
				continue
			}
			ready := true
			for p := range parents[t] {
				if !done[p] {
					ready = false
					break
				}
			}
			if ready {
				done[t] = true
				order = append(order, t)
				progressed = true
			}
		}
		if !progressed {
			var stuck []string
			for _, t := range tables {
				if !done[t] {
					stuck = append(stuck, t)
				}
			}
			return nil, fmt.Errorf("foreign-key cycle among %s", strings.Join(stuck, ", "))
		}
	}
	return order, nil
}

// Open returns one backup file's CSV: <table>.csv.gz or <table>.csv. A
// missing file is fs.ErrNotExist (os.ErrNotExist).
type Open func(table string) (io.ReadCloser, error)

// Restore loads a backup into the database at sdb, which must hold the
// current schema (start the API against it once). Every table is emptied
// first, then loaded in foreign-key order by the column names in each file's
// header, and every sequence is moved past its column's highest value, all in
// one transaction. Tables with no file (newer than the backup) stay empty.
// It returns the rows loaded per table.
func Restore(ctx context.Context, sdb *sql.DB, open Open, isMissing func(error) bool) (map[string]int64, error) {
	counts := map[string]int64{}
	conn, err := sdb.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	err = conn.Raw(func(dc any) error {
		sc, ok := dc.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("restore needs the pgx driver, got %T", dc)
		}
		tx, err := sc.Conn().BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit

		tables, err := listTables(ctx, tx)
		if err != nil {
			return err
		}
		ids := make([]string, len(tables))
		for i, t := range tables {
			ids[i] = pgx.Identifier{t}.Sanitize()
		}
		if _, err := tx.Exec(ctx, `TRUNCATE `+strings.Join(ids, ", ")+` RESTART IDENTITY`); err != nil {
			return fmt.Errorf("empty tables: %w", err)
		}
		for _, t := range tables {
			n, err := loadTable(ctx, tx.Conn().PgConn(), t, open)
			if isMissing(err) {
				log.Warn().Str("table", t).Msg("restore: no file, left empty")
				continue
			}
			if err != nil {
				return fmt.Errorf("load %s: %w", t, err)
			}
			counts[t] = n
		}
		// Serial and identity columns: next value after the highest restored.
		rows, err := tx.Query(ctx, `
			SELECT c.relname, a.attname, pg_get_serial_sequence(quote_ident(c.relname), a.attname)
			FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid
			WHERE c.relnamespace='public'::regnamespace AND c.relkind='r' AND a.attnum>0 AND NOT a.attisdropped
			  AND pg_get_serial_sequence(quote_ident(c.relname), a.attname) IS NOT NULL`)
		if err != nil {
			return err
		}
		type serial struct{ Table, Column, Seq string }
		serials, err := pgx.CollectRows(rows, pgx.RowToStructByPos[serial])
		if err != nil {
			return err
		}
		for _, s := range serials {
			col := pgx.Identifier{s.Column}.Sanitize()
			if _, err := tx.Exec(ctx, fmt.Sprintf(`SELECT setval($1, COALESCE(MAX(%s), 1), MAX(%s) IS NOT NULL) FROM %s`,
				col, col, pgx.Identifier{s.Table}.Sanitize()), s.Seq); err != nil {
				return fmt.Errorf("sequence %s: %w", s.Seq, err)
			}
		}
		return tx.Commit(ctx)
	})
	return counts, err
}

// loadTable copies one file into its table by the header's column names, so
// a column's position in the backup doesn't matter.
func loadTable(ctx context.Context, pg *pgconn.PgConn, table string, open Open) (int64, error) {
	f, err := open(table)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	line, err := br.ReadString('\n')
	if errors.Is(err, io.EOF) && line == "" {
		return 0, nil // empty file
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	cols, err := csv.NewReader(strings.NewReader(line)).Read()
	if err != nil {
		return 0, fmt.Errorf("header: %w", err)
	}
	for i, c := range cols {
		cols[i] = pgx.Identifier{c}.Sanitize()
	}
	tag, err := pg.CopyFrom(ctx, br, fmt.Sprintf(`COPY %s (%s) FROM STDIN WITH (FORMAT csv)`,
		pgx.Identifier{table}.Sanitize(), strings.Join(cols, ", ")))
	return tag.RowsAffected(), err
}

func restoreGuide(now time.Time, res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Sajni database backup, %s UTC\n\n%d tables, %d rows.\n\n", now.UTC().Format("2006-01-02 15:04"), res.Tables, res.Rows)
	b.WriteString("## Restore\n\n")
	b.WriteString("1. Download this folder (`gcloud storage cp -r gs://<bucket>/<this prefix> ./restore`).\n")
	b.WriteString("2. Start the API once against the new, empty database so it creates the current schema, then stop it.\n")
	b.WriteString("3. From `sajni-api`: `go run ./cmd/dbrestore -from ./restore -db '<new database URL>'`.\n")
	b.WriteString("   It empties every table, loads each file by column name in foreign-key order, resets the sequences,\n")
	b.WriteString("   all in one transaction, and prints rows per table to compare with the list below.\n\n")
	b.WriteString("| table | rows |\n| --- | ---: |\n")
	names := make([]string, 0, len(res.Counts))
	for t := range res.Counts {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		fmt.Fprintf(&b, "| %s | %d |\n", t, res.Counts[t])
	}
	return b.String()
}

// GzipOrPlain opens <dir-relative name>.csv.gz, else <name>.csv, through
// openFile; used by cmd/dbrestore and the round-trip test.
func GzipOrPlain(openFile func(name string) (io.ReadCloser, error), isMissing func(error) bool) Open {
	return func(table string) (io.ReadCloser, error) {
		f, err := openFile(table + ".csv.gz")
		if err == nil {
			zr, err := gzip.NewReader(f)
			if err != nil {
				f.Close()
				return nil, err
			}
			return readCloser{zr, func() error { zr.Close(); return f.Close() }}, nil
		}
		if !isMissing(err) {
			return nil, err
		}
		return openFile(table + ".csv")
	}
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }
