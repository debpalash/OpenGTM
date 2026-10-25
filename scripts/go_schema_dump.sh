#!/usr/bin/env bash
# Regenerate apps/backend-go/internal/db/schema.sql from the Alembic migrations.
# Requires: uv, docker, a running Postgres 18 (SCHEMA_DUMP_CONTAINER) reachable at DATABASE_URL.
#   ALEMBIC_DATABASE_URL  SQLAlchemy URL of the scratch DB (default: local test container)
#   PG_DUMP_URL           libpq URL of the same DB (default: local test container)
#   SCHEMA_DUMP_CONTAINER container whose pg_dump (v18) is used (default: opengtm-go-test-pg)
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
alembic_url="${ALEMBIC_DATABASE_URL:-postgresql+psycopg://postgres:postgres@127.0.0.1:55480/opengtm_test}"
dump_url="${PG_DUMP_URL:-postgres://postgres:postgres@127.0.0.1:5432/opengtm_test}"
container="${SCHEMA_DUMP_CONTAINER:-opengtm-go-test-pg}"
out="$root/apps/backend-go/internal/db/schema.sql"

(cd "$root/legacy/python" && DATABASE_URL="$alembic_url" uv run alembic upgrade head >&2)
# pg_dump runs inside the server container so client and server versions match.
docker exec "$container" pg_dump "$dump_url" --schema-only --no-owner --no-privileges \
  | grep -Ev '^(\\restrict|\\unrestrict) ' \
  | grep -Ev '^SET (statement_timeout|lock_timeout|idle_in_transaction_session_timeout|transaction_timeout|client_encoding|standard_conforming_strings|check_function_bodies|xmloption|client_min_messages|row_security) ' \
  | grep -Ev "^SELECT pg_catalog.set_config\('search_path'" \
  | cat -s > "$out"
echo "wrote $out" >&2
