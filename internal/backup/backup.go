// Package backup writes a weekly copy of the whole Postgres database to the
// blob bucket, so the "no data loss" rule has a backstop (the free database
// tier keeps no backups of its own).
//
// Each run writes backups/db/<UTC stamp>/<table>.csv.gz, one per public base
// table in Postgres' own COPY CSV format (NULLs and bytea round-trip), plus
// RESTORE.md with row counts and the restore recipe. A bucket lifecycle rule
// on the backups/ prefix deletes old runs (sajni-api/DEPLOY.md → Backups).
package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"sajni/internal/db"
	"sajni/internal/storage"
)

// Every is how often a backup runs; Hour is the UTC hour it may start in
// (21:00 UTC = 02:30 IST, the quietest time).
const (
	Every = 7 * 24 * time.Hour
	Hour  = 21
)

// Result says what one run wrote.
type Result struct {
	Prefix string
	Tables int
	Rows   int64
	Bytes  int
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

// Run dumps every table and records the result on the claimed row.
func Run(ctx context.Context, d *db.DB, store storage.Storage, id int64, now time.Time) (Result, error) {
	began := time.Now()
	res, err := dump(ctx, d, store, now)
	if err != nil {
		_, _ = d.ExecContext(ctx, `UPDATE db_backups SET error=$2 WHERE id=$1`, id, err.Error())
		return res, err
	}
	_, err = d.ExecContext(ctx, `UPDATE db_backups SET finished_at=$6, prefix=$2, tables=$3, row_count=$4, bytes=$5, error='' WHERE id=$1`,
		id, res.Prefix, res.Tables, res.Rows, res.Bytes, now.Add(time.Since(began)))
	return res, err
}

func dump(ctx context.Context, d *db.DB, store storage.Storage, now time.Time) (Result, error) {
	res := Result{Prefix: "backups/db/" + now.UTC().Format("20060102-150405") + "/"}
	conn, err := d.Conn(ctx)
	if err != nil {
		return res, err
	}
	defer conn.Close()

	var tables []string
	rows, err := conn.QueryContext(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return res, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	counts := make(map[string]int64, len(tables))
	// One repeatable-read snapshot, so every table is from the same moment.
	if _, err := conn.ExecContext(ctx, `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return res, err
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	for _, t := range tables {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		var n int64
		err := conn.Raw(func(dc any) error {
			pg := dc.(*stdlib.Conn).Conn().PgConn()
			tag, err := pg.CopyTo(ctx, zw, fmt.Sprintf(`COPY (SELECT * FROM %q) TO STDOUT WITH (FORMAT csv, HEADER true)`, t))
			n = tag.RowsAffected()
			return err
		})
		if err == nil {
			err = zw.Close()
		}
		if err != nil {
			return res, fmt.Errorf("copy %s: %w", t, err)
		}
		if err := store.Put(ctx, res.Prefix+t+".csv.gz", buf.Bytes(), "application/gzip"); err != nil {
			return res, fmt.Errorf("store %s: %w", t, err)
		}
		counts[t] = n
		res.Rows += n
		res.Bytes += buf.Len()
	}
	res.Tables = len(tables)
	manifest := restoreGuide(now, tables, counts, res.Rows)
	if err := store.Put(ctx, res.Prefix+"RESTORE.md", []byte(manifest), "text/markdown"); err != nil {
		return res, err
	}
	return res, nil
}

func restoreGuide(now time.Time, tables []string, counts map[string]int64, total int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Sajni database backup, %s UTC\n\n%d tables, %d rows.\n\n", now.UTC().Format("2006-01-02 15:04"), len(tables), total)
	b.WriteString("| table | rows |\n| --- | ---: |\n")
	for _, t := range tables {
		fmt.Fprintf(&b, "| %s | %d |\n", t, counts[t])
	}
	b.WriteString("\n## Restore\n\n")
	b.WriteString("1. Download this folder and `gunzip *.csv.gz`.\n")
	b.WriteString("2. Start the API once against an empty database to create the schema, then stop it.\n")
	b.WriteString("3. Load every table with foreign-key checks off:\n\n```sql\nBEGIN;\nSET session_replication_role = replica;\n")
	for _, t := range tables {
		fmt.Fprintf(&b, "TRUNCATE \"%s\" CASCADE;\n\\copy \"%s\" FROM '%s.csv' WITH (FORMAT csv, HEADER true)\n", t, t, t)
	}
	b.WriteString("SET session_replication_role = origin;\nCOMMIT;\n```\n\n")
	b.WriteString("4. Reset sequences: `SELECT setval(pg_get_serial_sequence(t, 'id'), MAX(id))` for each table with a serial id.\n")
	return b.String()
}
