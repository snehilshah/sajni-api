#!/usr/bin/env bash
# Full database backup via pg_dump -> timestamped, gzipped SQL dump.
#
#  DO NOT RUN automatically / in CI. Manual, on-demand backup only.
#  This is the restorable backup. All destructive ops go through a human.
#
# Usage:
#   DATABASE_URL=postgres://user:pass@host:5432/db ./backup-pgdump.sh [out_dir]
#
# Restore (manual, deliberate):
#   gunzip -c sajni-YYYYMMDD-HHMMSS.sql.gz | psql "$DATABASE_URL"
set -euo pipefail

: "${DATABASE_URL:?set DATABASE_URL to the Postgres connection string}"
OUT_DIR="${1:-./backups}"
mkdir -p "$OUT_DIR"
STAMP="$(date +%Y%m%d-%H%M%S)"
FILE="$OUT_DIR/sajni-$STAMP.sql.gz"

echo "Dumping database -> $FILE"
# --clean --if-exists make the dump idempotent to re-apply; --no-owner /
# --no-privileges keep it portable across roles (e.g. Cloud SQL).
pg_dump --no-owner --no-privileges --clean --if-exists "$DATABASE_URL" | gzip -9 > "$FILE"
echo "Done. $(du -h "$FILE" | cut -f1) written to $FILE"
