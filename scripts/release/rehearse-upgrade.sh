#!/usr/bin/env bash
# Upgrade rehearsal (release gate for issue #120): a REAL v3.0.0 (Python) installation is built from the v3.0.0
# tag's own code and migrations, seeded through its own API, and then upgraded to the Go runtime the supported way:
#
#   1. v3.0.0's Alembic builds the database (head 3f4051627384); v3's API creates an admin, a second workspace, a
#      saved provider key, an encrypted workspace secret, leads and a workbook function; a chat conversation and a
#      queue row are written with v3's own code (scripts/release/v3-seed.py), plus its SQLite data directory.
#   2. `docker compose run --rm migrate`           bridges 3f4051627384 -> baseline (internal/migrate) and adopts it.
#   3. `... migrate import-v3 --dry-run`, then for real: copies workspaces/settings/chat/functions from the SQLite files.
#   4. The Go stack starts; the SAME admin logs in with the v3 password (same SECRET_KEY) and every seeded item is
#      read back through the Go API.
#
#   scripts/release/rehearse-upgrade.sh [--python-ref REF]
#
# --python-ref other than v3.0.0 builds that ref instead (it must have Alembic and apps/api/main.py); the checks
# below are written for v3.0.0's data model. Details: docs/operations/release-rehearsals.md.
#
# Environment: see rehearse-install.sh; plus REHEARSAL_UV_IMAGE (default ghcr.io/astral-sh/uv:python3.12-bookworm-slim)
# for the container that runs the legacy Alembic and app.
set -uo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/rehearsal-lib.sh"

PY_REF="v3.0.0"
while (($#)); do
  case "$1" in
    --python-ref) PY_REF="${2:?--python-ref needs a git ref}"; shift 2 ;;
    -h|--help) sed -n '2,19p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
UV_IMAGE="${REHEARSAL_UV_IMAGE:-ghcr.io/astral-sh/uv:python3.12-bookworm-slim}"

rh_init "Upgrade rehearsal from $PY_REF"
START=$SECONDS
RH_USER="admin"
V3_PROVIDER_KEY="sk-ant-rehearsal-$(rh_rand 12)"
V3_WS_SECRET="hubspot-rehearsal-$(rh_rand 10)"
V3_APP_PASSWORD="$(rh_rand 16)"
SEEDJSON="$RH_WORK/v3data/seed.json"

# One v3 container run: the ref's tree copied in (the mount stays read-only), its locked deps, the data dir.
# $1 is the database login: "owner" for Alembic (DDL), "app" for the application. The application must NOT run as the
# superuser: v3.0.0 then refuses its Postgres lead store and silently keeps leads in per-workspace SQLite files, which
# is not how a real deployment (a non-super role under row-level security) behaves. Runs as the host user so the work
# dir can be removed afterwards.
v3_run() { # v3_run owner|app <shell command run in /tmp/work>
  local who="$1" cmd="$2" pw dbuser dbpass
  pw="$(grep '^POSTGRES_PASSWORD=' "$RH_ENV" | cut -d= -f2)"
  if [[ "$who" == "app" ]]; then dbuser="v3_app"; dbpass="$V3_APP_PASSWORD"; else dbuser="yupcha"; dbpass="$pw"; fi
  docker run --rm --user "$(id -u):$(id -g)" --network "${RH_PROJECT}_lead_net" -v "$RH_WORK/legacy:/src:ro" -v "$RH_WORK/v3data:/data" \
    -v "$RH_ROOT/scripts/release/v3-seed.py:/seed.py:ro" \
    -e "DATABASE_URL=postgresql+psycopg://$dbuser:$dbpass@postgres:5432/yupcha" \
    -e "SECRET_KEY=$(grep '^SECRET_KEY=' "$RH_ENV" | cut -d= -f2)" -e APP_ENV=local \
    -e "V3_ADMIN_PASSWORD=$RH_PASSWORD" -e "V3_PROVIDER_KEY=$V3_PROVIDER_KEY" -e "V3_WS_SECRET=$V3_WS_SECRET" \
    -e HOME=/tmp -e UV_CACHE_DIR=/tmp/uvcache -e UV_LINK_MODE=copy -e PYTHONPATH=/tmp/work "$UV_IMAGE" \
    sh -c "cp -r /src /tmp/work && cd /tmp/work && rm -rf data && ln -s /data data && uv sync --frozen --no-dev -q && $cmd"
}

legacy_database() {
  git -C "$RH_ROOT" rev-parse --verify --quiet "$PY_REF^{commit}" >/dev/null || { echo "git ref $PY_REF does not exist in this clone"; return 1; }
  mkdir -p "$RH_WORK/legacy" "$RH_WORK/v3data"
  git -C "$RH_ROOT" archive "$PY_REF" | tar -x -C "$RH_WORK/legacy" || return 1
  [[ -f "$RH_WORK/legacy/alembic.ini" ]] || { echo "$PY_REF has no alembic.ini: not a Python-era ref"; return 1; }
  chmod 755 "$RH_WORK/v3data"
  dc up -d postgres || return 1
  rh_wait 90 "postgres healthy" bash -c "[[ \"\$(docker inspect -f '{{.State.Health.Status}}' \$(docker compose -p '$RH_PROJECT' ps -q postgres) 2>/dev/null)\" == healthy ]]" || return 1
  v3_run owner 'uv run --frozen alembic upgrade head >/dev/null 2>&1; uv run --frozen alembic current 2>&1 | tail -n 1' \
    >"$RH_WORK/legacy-head.txt" 2>&1 || { echo "legacy alembic upgrade head failed:"; tail -n 20 "$RH_WORK/legacy-head.txt"; return 1; }
  # The login v3's application connects with in a real deployment: a non-super role in yupcha_app (created by Alembic).
  rh_psql "CREATE ROLE v3_app LOGIN PASSWORD '$V3_APP_PASSWORD' IN ROLE yupcha_app" >/dev/null || return 1
  RH_LEGACY_HEAD="$(rh_psql "SELECT version_num FROM alembic_version")"
  echo "$PY_REF built a Python-era database with $(rh_psql "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'") tables at Alembic revision $RH_LEGACY_HEAD"
  [[ -n "$RH_LEGACY_HEAD" ]]
}

seed_v3() {
  v3_run app 'uv run --frozen python /seed.py' >"$RH_WORK/seed.out" 2>&1 || { echo "v3 seeding failed:"; tail -n 25 "$RH_WORK/seed.out"; return 1; }
  tail -n 1 "$RH_WORK/seed.out"
  [[ -s "$SEEDJSON" ]] || { echo "seed.json missing"; return 1; }
  echo "v3 data dir: $(cd "$RH_WORK/v3data" && ls ./*.db 2>/dev/null | tr '\n' ' ')"
  # A default install keeps leads, outreach and signals in PostgreSQL: no SQLite fallback file may exist.
  if ls "$RH_WORK"/v3data/leads.db "$RH_WORK"/v3data/outreach.db "$RH_WORK"/v3data/signals.db "$RH_WORK"/v3data/workspaces/*/leads.db >/dev/null 2>&1; then
    echo "a SQLite lead/outreach/signal store was created: the seed did not follow the default PostgreSQL path"; return 1
  fi
  echo "v3 PostgreSQL rows: leads=$(rh_psql 'SELECT count(*) FROM leads') outreach_sequences=$(rh_psql 'SELECT count(*) FROM outreach_sequences') signals=$(rh_psql 'SELECT count(*) FROM signals')"
  echo "v3 rows: users=$(rh_psql 'SELECT count(*) FROM users') jobs=$(rh_psql 'SELECT count(*) FROM jobs')"
}

go_migrator_bridges() {
  if ! dc run --rm migrate >"$RH_WORK/migrate.out" 2>&1; then
    echo "the Go migrator failed (Alembic head was $RH_LEGACY_HEAD):"
    grep -v "Container \|Network \|Volume " "$RH_WORK/migrate.out" | tail -n 10
    return 1
  fi
  grep -v "Container \|Network \|Volume " "$RH_WORK/migrate.out" | tail -n 4
  echo "goose_db_version: $(rh_psql "SELECT max(version_id) FROM goose_db_version"); alembic_version: $(rh_psql "SELECT version_num FROM alembic_version")"
  [[ "$(rh_psql "SELECT version_num FROM alembic_version")" == "e7b2c4d6f8a0" ]] || { echo "the bridge did not stamp e7b2c4d6f8a0"; return 1; }
  local py; py="$(rh_psql "SELECT count(*) FROM jobs WHERE status='pending' AND executor='python'")"
  [[ "$py" == "0" ]] || { echo "$py pending job(s) still have executor 'python' after the bridge"; return 1; }
  echo "queue: pending jobs=$(rh_psql "SELECT count(*) FROM jobs WHERE status='pending' AND executor='go'") (all executor go), completed kept=$(rh_psql "SELECT count(*) FROM jobs WHERE status='completed'")"
}

import_v3() { # import_v3 [--dry-run]
  dc run --rm -v "$RH_WORK/v3data:/v3data:ro" migrate import-v3 --data-dir /v3data "$@" 2>&1 | grep -v "Container \|Network \|Volume "
  return "${PIPESTATUS[0]}"
}
import_dry() { import_v3 --dry-run; }
import_real() {
  import_v3 || return 1
  echo "--- second run (must insert nothing) ---"
  import_v3 || return 1
}

seedv() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$SEEDJSON" "$1"; }
has() { grep -qF -- "$2" <<<"$1"; }

verify_through_go_api() {
  local acme conv body
  acme="$(seedv acme_ws)"; conv="$(seedv conversation_id)"
  body="$(rh_api /api/workspaces)" || return 1
  has "$body" "$acme" || { echo "workspace $acme is not listed: $(head -c 300 <<<"$body")"; return 1; }
  echo "workspaces: the v3 workspace $acme is listed"
  body="$(rh_api /api/leads -H "X-Workspace-Id: $acme")" || return 1
  has "$body" "Rehearsal Co" || { echo "lead 'Rehearsal Co' not visible in workspace $acme: $(head -c 300 <<<"$body")"; return 1; }
  has "$body" "Main Workspace Co" && { echo "lead of the other workspace leaked into $acme"; return 1; }
  echo "leads: 'Rehearsal Co' visible in its workspace, the other workspace's lead is not"
  body="$(rh_api /api/outreach/sequences -H "X-Workspace-Id: $acme")" || return 1
  has "$body" "Rehearsal sequence" || { echo "outreach sequence not listed: $(head -c 300 <<<"$body")"; return 1; }
  [[ "$(rh_psql "SELECT (SELECT count(*) FROM outreach_enrollments) || '/' || (SELECT count(*) FROM signals)")" == "1/1" ]] \
    || { echo "enrollment/signal counts are not 1/1"; return 1; }
  echo "outreach: the sequence is listed, its enrollment and the signal survived (kept in PostgreSQL by the bridge)"
  body="$(rh_api /api/functions)" || return 1
  has "$body" "Rehearsal function" || { echo "function not listed: $(head -c 300 <<<"$body")"; return 1; }
  echo "functions: 'Rehearsal function' listed"
  body="$(rh_api /api/copilotkit/conversations -H "X-Workspace-Id: $acme")" || return 1
  has "$body" "$conv" || { echo "conversation $conv not listed: $(head -c 300 <<<"$body")"; return 1; }
  body="$(rh_api "/api/copilotkit/conversations/$conv" -H "X-Workspace-Id: $acme")" || return 1
  { has "$body" "hello from v3" && has "$body" "hello back"; } || { echo "chat messages missing: $(head -c 300 <<<"$body")"; return 1; }
  echo "chat: conversation and both messages readable"
  body="$(rh_api /api/settings/providers)" || return 1
  has "$body" "${V3_PROVIDER_KEY:0:8}" || { echo "saved provider key is not reported as configured: $(head -c 400 <<<"$body")"; return 1; }
  echo "providers: the saved anthropic key is read back (masked)"
  body="$(rh_api /api/settings/workspace-integrations -H "X-Workspace-Id: $acme")" || return 1
  has "$body" "hubspot" || { echo "hubspot integration not listed: $(head -c 400 <<<"$body")"; return 1; }
  has "$body" "$V3_WS_SECRET" && { echo "workspace secret returned in clear text"; return 1; }
  echo "workspace secret: hubspot integration listed and the value is not echoed"
}

rh_step "build or reuse the stack images"                                  rh_build
rh_step "build a Python-era database with $PY_REF's own Alembic"           legacy_database
rh_step "seed it through $PY_REF's own API and code"                       seed_v3
rh_step "Go migrator bridges and adopts the database"                      go_migrator_bridges
rh_step "import-v3 --dry-run writes nothing and reports"                   import_dry
rh_step "import-v3 copies the SQLite stores (twice: idempotent)"           import_real
rh_step "start the stack on the upgraded database"                         rh_up
rh_step "stack becomes healthy"                                            rh_wait_healthy
rh_step "the v3 admin logs in with the v3 password"                        rh_login
rh_step "every seeded item is visible through the Go API"                  verify_through_go_api
rh_step "UI index is served"                                               rh_check_ui
rh_info "elapsed $((SECONDS - START))s"
rh_finish
