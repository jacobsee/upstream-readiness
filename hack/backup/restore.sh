#!/usr/bin/env bash
# Restores a backup made by backup.sh, replacing all local state.
#
#   hack/backup/restore.sh [--yes] <backup-dir>
#
# Stop the orchestrator and plugins first; it refuses while they're connected.
# Deletes every docker compose volume, Temporal's included, after saving a
# safety backup to backups/pre-restore-<UTC timestamp>. Runs that were in
# flight in the backup come back failed, and can be rerun from a step.
# workflows/ is compared with the backup but never overwritten.
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

YES=no
if [ "${1:-}" = "--yes" ]; then
  YES=yes
  shift
fi
if [ $# -ne 1 ]; then
  echo "usage: $0 [--yes] <backup-dir>" >&2
  exit 2
fi
BACKUP="$(cd "$1" && pwd)"

if [ ! -f "$BACKUP/SHA256SUMS" ]; then
  echo "error: $BACKUP has no SHA256SUMS; not a finished backup" >&2
  exit 1
fi
if ! (cd "$BACKUP" && shasum -a 256 --status -c SHA256SUMS); then
  echo "error: $BACKUP failed its checksum verification" >&2
  exit 1
fi
for entry in $DBS; do
  split_db "$entry"
  if [ ! -f "$BACKUP/$DB.dump" ]; then
    echo "error: $BACKUP has no dump of $DB" >&2
    exit 1
  fi
done

echo "restoring $BACKUP"
cat "$BACKUP/manifest.txt"
echo

# shellcheck disable=SC2046 # one service per word
compose up -d $(db_services)
wait_ready

busy=no
for entry in $DBS; do
  split_db "$entry"
  conns="$(psql_val "$SVC" "$USER_" "$DB" "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type = 'client backend'")"
  if [ "$conns" != 0 ]; then
    echo "error: $conns client connection(s) to $DB" >&2
    busy=yes
  fi
done
if [ "$busy" = yes ]; then
  echo "error: stop the orchestrator and plugins, then run this again" >&2
  exit 1
fi

if [ "$YES" != yes ]; then
  echo "This deletes every docker compose volume, Temporal's included."
  printf 'Type "restore" to continue: '
  read -r answer || answer=""
  if [ "$answer" != restore ]; then
    echo "aborted" >&2
    exit 1
  fi
fi

SAFETY="$ROOT/backups/pre-restore-$(date -u +%Y%m%dT%H%M%SZ)"
"$(dirname "${BASH_SOURCE[0]}")/backup.sh" "$SAFETY"
echo
echo "if anything below fails, the previous state is in $SAFETY"
echo

compose down -v
# shellcheck disable=SC2046 # one service per word
compose up -d $(db_services)
wait_ready

for entry in $DBS; do
  split_db "$entry"
  echo "restoring $DB ($SVC)"
  compose exec -T "$SVC" pg_restore --exit-on-error --single-transaction --no-owner -U "$USER_" -d "$DB" < "$BACKUP/$DB.dump"
done

# Temporal starts empty, so nothing will ever finish these runs. Left as
# "running" they could never be cancelled, rerun or purged.
closed="$(psql_val "$ORCH_SVC" "$ORCH_USER" "$ORCH_DB" "
  WITH steps AS (
    UPDATE run_steps
       SET status = 'cancelled', completed_at = now(), error = 'interrupted by restore', prompt_blob = NULL
     WHERE status IN ('running', 'awaiting_resolution')
  ), runs AS (
    UPDATE run_summaries SET status = 'failed', completed_at = now()
     WHERE status = 'running'
    RETURNING 1
  )
  SELECT count(*) FROM runs
")"

compose up -d

echo
echo "restore complete; marked $closed in-flight run(s) failed"
if ! diff -rq "$BACKUP/workflows" "$ROOT/workflows"; then
  echo "workflows/ differs from the backup (not overwritten); the backup's copy is in $BACKUP/workflows"
fi
echo "next: start the plugins and the orchestrator, from the commit in the manifest or a newer one"
