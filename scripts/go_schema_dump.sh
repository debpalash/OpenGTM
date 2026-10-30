#!/usr/bin/env bash
# Regenerate apps/backend-go/internal/db/schema.sql (the sqlc input) from the Go migrations:
# run opengtm-migrate (go run ./cmd/migrate) on a scratch database, pg_dump it, normalize.
#   scripts/go_schema_dump.sh           rewrite schema.sql
#   scripts/go_schema_dump.sh --check   exit 1 if schema.sql differs from the migrations
# Native Postgres alternative: SCHEMA_DUMP_DATABASE_URL=postgres://owner@host/postgres.
# Requires: go and either native PostgreSQL tools or docker and a running Postgres 18 container (pg_dump runs inside it so client
# and server versions match). No Python, no Alembic.
#   SCHEMA_DUMP_CONTAINER  container (default: opengtm-go-test-pg)
#   SCHEMA_DUMP_PORT       its published host port (default: 55480)
#   SCHEMA_DUMP_HOST       host address (default: 127.0.0.1)
# The superuser password is read from the container's POSTGRES_PASSWORD and never printed.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
container="${SCHEMA_DUMP_CONTAINER:-opengtm-go-test-pg}"
port="${SCHEMA_DUMP_PORT:-55480}"
host="${SCHEMA_DUMP_HOST:-127.0.0.1}"
out="$root/apps/backend-go/internal/db/schema.sql"
check=0
[[ "${1:-}" == "--check" ]] && check=1

db="opengtm_schema_dump_$$"
tmp="$(mktemp)"
gen="$(mktemp)"
cleanup() {
  if [[ -n "${SCHEMA_DUMP_DATABASE_URL:-}" ]]; then
    dropdb --maintenance-db="$SCHEMA_DUMP_DATABASE_URL" --if-exists "$db" >/dev/null 2>&1 || true
  else
    docker exec "$container" dropdb -U postgres --if-exists "$db" >/dev/null 2>&1 || true
  fi
  rm -f "$tmp" "$gen" "$tmp.raw"
}
trap cleanup EXIT
if [[ -n "${SCHEMA_DUMP_DATABASE_URL:-}" ]]; then
  createdb --maintenance-db="$SCHEMA_DUMP_DATABASE_URL" "$db"
  task_dsn="$(python3 - "$db" <<'PYURL'
import os,sys,urllib.parse
u=urllib.parse.urlsplit(os.environ['SCHEMA_DUMP_DATABASE_URL'])
print(urllib.parse.urlunsplit(u._replace(path='/'+sys.argv[1])))
PYURL
)"
  DATABASE_URL="$task_dsn" bash -c 'cd "$1/apps/backend-go" && go run ./cmd/migrate up' _ "$root" >&2
  pg_dump --dbname="$task_dsn" --schema-only --no-owner --no-privileges --exclude-table=goose_db_version > "$tmp.raw"
else
  docker exec "$container" createdb -U postgres "$db"
  PGPASSWORD="$(docker exec "$container" printenv POSTGRES_PASSWORD)" \
  DATABASE_URL="postgres://postgres@${host}:${port}/${db}" \
    bash -c 'cd "$1/apps/backend-go" && go run ./cmd/migrate up' _ "$root" >&2
  docker exec "$container" pg_dump -U postgres -d "$db" --schema-only --no-owner --no-privileges --exclude-table=goose_db_version > "$tmp.raw"
fi
cat "$tmp.raw" \
  | grep -Ev '^(\\restrict|\\unrestrict) ' \
  | grep -Ev '^SET (statement_timeout|lock_timeout|idle_in_transaction_session_timeout|transaction_timeout|client_encoding|standard_conforming_strings|check_function_bodies|xmloption|client_min_messages|row_security) ' \
  | grep -Ev "^SELECT pg_catalog.set_config\('search_path'" \
  | cat -s > "$tmp"

# --no-privileges drops every ACL, but outreach_send_by_message_id (SECURITY DEFINER lookup used by
# the inbound webhooks) must not be executable by PUBLIC. Re-append that REVOKE/GRANT right after the
# function body, byte-identical on every run; fail if the function is missing from the dump.
fn='public.outreach_send_by_message_id(p_message_id text)'
awk -v fn="$fn" '
  { lines[++n] = $0 }
  state == 0 && index($0, "CREATE FUNCTION public.outreach_send_by_message_id(") == 1 { state = 1; next }
  state == 1 && $0 ~ /^[ \t]*\$\$;$/ {
    lines[++n] = ""
    lines[++n] = "REVOKE ALL ON FUNCTION " fn " FROM PUBLIC;"
    lines[++n] = ""
    lines[++n] = "DO $$"
    lines[++n] = "BEGIN"
    lines[++n] = "    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '\''yupcha_app'\'') THEN"
    lines[++n] = "        GRANT EXECUTE ON FUNCTION " fn " TO yupcha_app;"
    lines[++n] = "    END IF;"
    lines[++n] = "END"
    lines[++n] = "$$;"
    state = 2
  }
  END {
    if (state != 2) { print "outreach_send_by_message_id not found in dump" > "/dev/stderr"; exit 1 }
    for (i = 1; i <= n; i++) print lines[i]
  }' "$tmp" > "$gen"

if [[ $check == 1 ]]; then
  if ! diff -u "$out" "$gen" >&2; then
    echo "schema.sql is out of date with the Go migrations: run make schema" >&2
    exit 1
  fi
  echo "schema.sql matches the Go migrations" >&2
else
  cp "$gen" "$out"
  echo "wrote $out" >&2
fi
