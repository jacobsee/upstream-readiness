#!/usr/bin/env bash
# Backs up instance configuration, run history and plugin memory.
#
#   hack/backup/backup.sh [output-dir]
#
# Creates (default output-dir: backups/<UTC timestamp>):
#   <database>.dump  pg_dump custom format, one per entry in DBS (lib.sh)
#   workflows/       copy of the workflow files, uncommitted edits included
#   manifest.txt     when, which commit, schema versions, runs in flight
#   SHA256SUMS       checked by restore.sh; absent means an unfinished backup
#
# Temporal and the plugins' work dirs are left out on purpose (see "Backup
# and restore" in the README). The output contains SSH private keys and is
# not encrypted.
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

OUT="${1:-$ROOT/backups/$(date -u +%Y%m%dT%H%M%SZ)}"
OUT="${OUT%/}"

# A database nobody listed would otherwise be skipped silently.
for svc in $(compose config --services | grep postgres); do
  [ "$svc" = temporal-postgres ] && continue
  if ! db_services | grep -qx "$svc"; then
    echo "error: compose service $svc is not in DBS (hack/backup/lib.sh); it would not be backed up" >&2
    exit 1
  fi
done

mkdir -p "$(dirname "$OUT")"
mkdir "$OUT"

# shellcheck disable=SC2046 # one service per word
compose up -d $(db_services)
wait_ready

{
  echo "created_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "git_commit: $(git -C "$ROOT" rev-parse HEAD)"
  echo "git_dirty: $([ -n "$(git -C "$ROOT" status --porcelain)" ] && echo yes || echo no)"
} > "$OUT/manifest.txt"

for entry in $DBS; do
  split_db "$entry"
  echo "dumping $DB ($SVC)"
  compose exec -T "$SVC" pg_dump -Fc -U "$USER_" "$DB" > "$OUT/$DB.dump.tmp"
  compose exec -T "$SVC" pg_restore --list < "$OUT/$DB.dump.tmp" > /dev/null
  mv "$OUT/$DB.dump.tmp" "$OUT/$DB.dump"
  version="$(table_val "$SVC" "$USER_" "$DB" schema_version 'SELECT COALESCE(MAX(version), 0) FROM schema_version')"
  echo "schema_version_$DB: $version" >> "$OUT/manifest.txt"
done

cp -R "$ROOT/workflows" "$OUT/workflows"

in_flight="$(table_val "$ORCH_SVC" "$ORCH_USER" "$ORCH_DB" run_summaries "SELECT count(*) FROM run_summaries WHERE status = 'running'")"
echo "in_flight_runs: $in_flight" >> "$OUT/manifest.txt"

(cd "$OUT" && find . -type f ! -name SHA256SUMS -exec shasum -a 256 {} + > SHA256SUMS)

echo
echo "backup written to $OUT"
if [ "$in_flight" != 0 ]; then
  echo "warning: $in_flight run(s) in flight; a restore of this backup marks them failed"
fi
echo "warning: this backup contains SSH private keys and is not encrypted"
