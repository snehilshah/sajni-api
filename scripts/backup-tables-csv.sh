#!/usr/bin/env bash
# Per-table CSV export -> one .csv per table in a timestamped folder.
#
#  DO NOT RUN automatically / in CI. Manual, on-demand only.
#  Human-readable snapshot for inspection / diffing. NOT a restore path —
#  use backup-pgdump.sh when you need to restore.
#
# Usage:
#   DATABASE_URL=postgres://user:pass@host:5432/db ./backup-tables-csv.sh [out_dir]
set -euo pipefail

: "${DATABASE_URL:?set DATABASE_URL to the Postgres connection string}"
OUT_DIR="${1:-./backups}/csv-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT_DIR"

# Every base table in the public schema.
TABLES="$(psql "$DATABASE_URL" -At -c \
  "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename")"

for t in $TABLES; do
  echo "-> $t"
  psql "$DATABASE_URL" -c "\copy (SELECT * FROM \"$t\") TO '$OUT_DIR/$t.csv' WITH CSV HEADER"
done

echo "Done. $(ls -1 "$OUT_DIR" | wc -l) tables -> $OUT_DIR"
