#!/usr/bin/env bash
# Upgrade rehearsal (release gate for issue #120): build a real Python-era database from a legacy git ref
# (default v3.0.0, the only published release) by running THAT ref's own Alembic migrations, then run the Go
# migrator (`opengtm-migrate up`, the compose `migrate` service) against it, and start the stack on the result.
#
#   scripts/release/rehearse-upgrade.sh [--python-ref REF]
#
# EXPECTED RESULT TODAY: FAIL at "Go migrator adopts the Python-era database" for v3.0.0. v3.0.0's Alembic head
# is 3f4051627384, but the Go runner adopts only e7b2c4d6f8a0, and the Python revisions between the two live in
# a commit (86f41c6, "legacy/python") that is not in this repository or on any ref. The script reports that
# honestly rather than faking a fixture; it turns green only when a ref whose Alembic head is e7b2c4d6f8a0 is
# available (pass it with --python-ref). Details: docs/operations/release-rehearsals.md.
#
# Steps after adoption (stack start, /healthz, UI) have never been reached on a real Python-era database,
# because no such database can be adopted yet; they use the same helpers as rehearse-install.sh.
#
# Environment: see rehearse-install.sh; plus REHEARSAL_UV_IMAGE (default ghcr.io/astral-sh/uv:python3.12-bookworm-slim)
# for the container that runs the legacy Alembic.
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
ADOPTABLE="e7b2c4d6f8a0"

rh_init "Upgrade rehearsal from $PY_REF"
START=$SECONDS

legacy_database() {
  git -C "$RH_ROOT" rev-parse --verify --quiet "$PY_REF^{commit}" >/dev/null || { echo "git ref $PY_REF does not exist in this clone"; return 1; }
  mkdir -p "$RH_WORK/legacy"
  git -C "$RH_ROOT" archive "$PY_REF" | tar -x -C "$RH_WORK/legacy" || return 1
  [[ -f "$RH_WORK/legacy/alembic.ini" ]] || { echo "$PY_REF has no alembic.ini: not a Python-era ref"; return 1; }
  dc up -d postgres || return 1
  rh_wait 90 "postgres healthy" bash -c "[[ \"\$(docker inspect -f '{{.State.Health.Status}}' \$(docker compose -p '$RH_PROJECT' ps -q postgres) 2>/dev/null)\" == healthy ]]" || return 1
  local pw; pw="$(grep '^POSTGRES_PASSWORD=' "$RH_ENV" | cut -d= -f2)"
  # The legacy Alembic runs from the legacy tree (copied inside the container so the mount stays read-only),
  # against this project's postgres over its network, with the ref's own locked dependencies.
  docker run --rm --network "${RH_PROJECT}_lead_net" -v "$RH_WORK/legacy:/src:ro" \
    -e "DATABASE_URL=postgresql+psycopg://yupcha:$pw@postgres:5432/yupcha" \
    -e "SECRET_KEY=$(grep '^SECRET_KEY=' "$RH_ENV" | cut -d= -f2)" -e UV_LINK_MODE=copy "$UV_IMAGE" \
    sh -c 'cp -r /src /work && cd /work && uv sync --frozen --no-dev -q && uv run --frozen alembic upgrade head >/dev/null 2>&1 && uv run --frozen alembic current 2>&1 | tail -n 1' \
    >"$RH_WORK/legacy-head.txt" 2>&1 || { echo "legacy alembic upgrade head failed:"; tail -n 20 "$RH_WORK/legacy-head.txt"; return 1; }
  RH_LEGACY_HEAD="$(rh_psql "SELECT version_num FROM alembic_version")"
  echo "$PY_REF built a Python-era database with $(rh_psql "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'") tables at Alembic revision $RH_LEGACY_HEAD"
  [[ -n "$RH_LEGACY_HEAD" ]]
}

go_migrator_adopts() {
  if ! dc run --rm migrate >"$RH_WORK/migrate.out" 2>&1; then
    echo "the Go migrator refused the database (Alembic head $RH_LEGACY_HEAD, the runner adopts only $ADOPTABLE):"
    grep -v "Container \|Network \|Volume " "$RH_WORK/migrate.out" | tail -n 6
    return 1
  fi
  echo "migrate up exited 0; goose_db_version: $(rh_psql "SELECT max(version_id) FROM goose_db_version")"
}

rh_step "build or reuse the stack images"                                  rh_build
rh_step "build a Python-era database with $PY_REF's own Alembic"           legacy_database
rh_step "Go migrator adopts the Python-era database"                       go_migrator_adopts
rh_step "start the stack on the upgraded database"                         rh_up
rh_step "stack becomes healthy"                                            rh_wait_healthy
rh_step "UI index is served"                                               rh_check_ui
rh_step "/healthz"                                                         rh_check_healthz
rh_info "elapsed $((SECONDS - START))s"
rh_finish
