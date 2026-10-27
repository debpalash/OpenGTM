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
tmp="$(mktemp)"

(cd "$root/legacy/python" && DATABASE_URL="$alembic_url" uv run alembic upgrade head >&2)
# pg_dump runs inside the server container so client and server versions match.
docker exec "$container" pg_dump "$dump_url" --schema-only --no-owner --no-privileges \
  | grep -Ev '^(\\restrict|\\unrestrict) ' \
  | grep -Ev '^SET (statement_timeout|lock_timeout|idle_in_transaction_session_timeout|transaction_timeout|client_encoding|standard_conforming_strings|check_function_bodies|xmloption|client_min_messages|row_security) ' \
  | grep -Ev "^SELECT pg_catalog.set_config\('search_path'" \
  | cat -s > "$tmp"
# --no-privileges drops every ACL, but outreach_send_by_message_id (SECURITY DEFINER lookup used by
# the inbound webhooks) must not be executable by PUBLIC. Re-append that REVOKE/GRANT right after the
# function body, byte-identical on every run; fail if the function is missing from the dump.
python3 - "$tmp" "$out" <<'PY'
import re, sys
src, dst = sys.argv[1:3]
lines = open(src).read().split("\n")
fn = "public.outreach_send_by_message_id(p_message_id text)"
extra = f"""
REVOKE ALL ON FUNCTION {fn} FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'yupcha_app') THEN
        GRANT EXECUTE ON FUNCTION {fn} TO yupcha_app;
    END IF;
END
$$;""".split("\n")
out, state = [], 0
for ln in lines:
    out.append(ln)
    if state == 0 and ln.startswith("CREATE FUNCTION public.outreach_send_by_message_id("):
        state = 1
    elif state == 1 and re.fullmatch(r"\s*\$\$;", ln):
        out.extend(extra)
        state = 2
if state != 2:
    sys.exit("outreach_send_by_message_id not found in dump")
open(dst, "w").write("\n".join(out))
PY
rm -f "$tmp"
echo "wrote $out" >&2
