# Shared by backup.sh and restore.sh. Source it; don't run it.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Every database worth keeping, as service:user:database. A plugin with its
# own Postgres adds one entry here; backup.sh fails until it does.
DBS="postgres:orchestrator:orchestrator godep-postgres:godep:godep rebase-postgres:rebase:rebase"

# Holds the orchestrator's run tables, which restore.sh has to close.
ORCH_SVC=postgres ORCH_USER=orchestrator ORCH_DB=orchestrator

# Dumps hold SSH private keys (plugin_instances.config_blob).
umask 077

compose() { docker compose -f "$ROOT/docker-compose.yml" "$@"; }

db_services() {
  for entry in $DBS; do echo "${entry%%:*}"; done
}

# split_db service:user:database sets SVC, USER_ and DB.
split_db() {
  IFS=: read -r SVC USER_ DB <<EOF
$1
EOF
}

# Over TCP on purpose: during first-time init the image's temporary server
# listens on the unix socket only, so a plain pg_isready passes too early.
wait_ready() {
  local entry tries
  for entry in $DBS; do
    split_db "$entry"
    tries=0
    until compose exec -T "$SVC" pg_isready -q -h 127.0.0.1 -U "$USER_" -d "$DB" >/dev/null 2>&1; do
      tries=$((tries + 1))
      if [ "$tries" -ge 60 ]; then
        echo "error: $SVC not ready after 60s" >&2
        return 1
      fi
      sleep 1
    done
  done
}

# psql_val service user database sql prints the single result value.
psql_val() {
  compose exec -T "$1" psql -v ON_ERROR_STOP=1 -U "$2" -d "$3" -Atc "$4"
}

# table_val service user database table sql is psql_val, printing 0 when the
# table doesn't exist yet (a volume nothing has migrated).
table_val() {
  if [ "$(psql_val "$1" "$2" "$3" "SELECT to_regclass('$4') IS NOT NULL")" = "t" ]; then
    psql_val "$1" "$2" "$3" "$5"
  else
    echo 0
  fi
}
