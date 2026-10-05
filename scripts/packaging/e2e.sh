#!/usr/bin/env bash
# End-to-end proof of the M7 exit gate on one machine and one architecture:
# fresh install, backup and restore (in place and onto a new directory, as in
# disaster recovery), upgrade from the previous release, failed upgrade with
# automatic rollback, and manual rollback. CI runs it on amd64
# and arm64 (.github/workflows/packaging.yml); run it locally the same way:
#
#   OPENGTM_BIN=./opengtm \
#   PREV_APP_IMAGE=ghcr.io/debpalash/opengtm:3.0.0 ...   # or locally built tags
#   scripts/packaging/e2e.sh
#
# It uses its own Compose project and a random high port, and removes every
# container, volume and temporary directory it creates, so it is safe next to
# a running OpenGTM stack. It never reads or writes ./data of any other install.
#
# Inputs (environment):
#   OPENGTM_BIN          opengtm binary to test (required)
#   E2E_PROFILE          lite | standard | full (default lite)
#   PREV_VERSION         the release being upgraded from      (default 1.0.0)
#   PREV_APP_IMAGE       its Python image                      (required)
#   PREV_SERVER_IMAGE    its Go server image                   (required)
#   NEXT_VERSION         the release being upgraded to         (default 2.0.0)
#   NEXT_APP_IMAGE       its Python image                      (required)
#   NEXT_SERVER_IMAGE    its Go server image                   (required)
#   NO_PULL              1 for locally built images (default 1; set 0 for registry images)
#                        For the upgrade to cross real schema revisions, PREV_APP_IMAGE
#                        must be an image whose migrations are older than NEXT's
#                        (CI uses the latest published release; locally build one
#                        from an older commit).
#   E2E_KEEP             1 to keep the install directory and containers on exit
#   E2E_LOG_DIR          directory that receives `docker compose logs` when the run fails
set -euo pipefail

: "${OPENGTM_BIN:?set OPENGTM_BIN to the opengtm binary}"
: "${PREV_APP_IMAGE:?}" "${PREV_SERVER_IMAGE:?}" "${NEXT_APP_IMAGE:?}" "${NEXT_SERVER_IMAGE:?}"
PROFILE="${E2E_PROFILE:-lite}"
PREV_VERSION="${PREV_VERSION:-1.0.0}"
NEXT_VERSION="${NEXT_VERSION:-2.0.0}"
NO_PULL="${NO_PULL:-1}"

OPENGTM_BIN="$(cd "$(dirname "$OPENGTM_BIN")" && pwd)/$(basename "$OPENGTM_BIN")"
SUFFIX="$(python3 -c 'import secrets; print(secrets.token_hex(4))')"
PROJECT="m7e2e-${SUFFIX}"
PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
PY
)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/opengtm-e2e.XXXXXX")"
DIR="$WORK/install"
DR="$WORK/recovered"
BASE="http://127.0.0.1:${PORT}"
PULL_FLAG=()
[[ "$NO_PULL" == "1" ]] && PULL_FLAG=(--no-pull)

step() { printf '\n=== %s\n' "$*"; }
fail() { printf 'E2E FAILED: %s\n' "$*" >&2; exit 1; }
dc() { docker compose --project-directory "$DIR" -f "$DIR/compose.yml" "$@"; }
og() { "$OPENGTM_BIN" "$@"; }
# The single quotes are intentional: $POSTGRES_* must expand inside the container.
# shellcheck disable=SC2016
psql_owner() { dc exec -T postgres sh -c 'psql -X -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"' _ "$1"; }
version_json() { curl -fsS "$BASE/api/v2/version"; }

cleanup() {
  local code=$?
  if [[ "${E2E_KEEP:-0}" == "1" ]]; then
    echo "E2E_KEEP=1: leaving $DIR (project $PROJECT, port $PORT)"
    return
  fi
  if [[ $code -ne 0 && -n "${E2E_LOG_DIR:-}" && -d "$DIR" ]]; then
    mkdir -p "$E2E_LOG_DIR"
    dc logs --no-color > "$E2E_LOG_DIR/compose.log" 2>&1 || true
    dc ps -a > "$E2E_LOG_DIR/compose-ps.txt" 2>&1 || true
  fi
  if [[ -d "$DIR" ]]; then
    dc down -v --remove-orphans --timeout 5 >/dev/null 2>&1 || true
  fi
  if [[ -d "$DR" ]]; then
    docker compose --project-directory "$DR" -f "$DR/compose.yml" down -v --remove-orphans --timeout 5 >/dev/null 2>&1 || true
  fi
  # Containers write into data/ as another uid on some hosts.
  rm -rf "$WORK" 2>/dev/null \
    || docker run --rm -v "$WORK:/w" --entrypoint rm "$PREV_APP_IMAGE" -rf /w/install 2>/dev/null || true
  rm -rf "$WORK" 2>/dev/null || true
  exit "$code"
}
trap cleanup EXIT

assert_healthy() {
  local want="$1"
  curl -fsS "$BASE/readyz" >/dev/null || fail "readyz is not OK ($1)"
  curl -fsS "$BASE/health" >/dev/null || fail "/health through the front door is not OK ($1)"
  [[ "$(version_json)" == *"\"version\":\"${want}\""* ]] || fail "server does not report version ${want}: $(version_json)"
}

marker() { psql_owner "SELECT string_agg(note, ',' ORDER BY id) FROM e2e_marker"; }
head_rev() { psql_owner "SELECT version_num FROM alembic_version"; }

step "version and usage"
og version
usage_text="$(og 2>&1 || true)"
[[ "$usage_text" == *upgrade* ]] || fail "usage lacks upgrade"

step "1/10 fresh install (profile $PROFILE, previous release $PREV_VERSION)"
og init --dir "$DIR" --yes --profile "$PROFILE" --project "$PROJECT" --port "$PORT" \
  --version "$PREV_VERSION" --app-image "$PREV_APP_IMAGE" --server-image "$PREV_SERVER_IMAGE" --start
assert_healthy "$PREV_VERSION"
[[ "$(stat -c %a "$DIR/.env" 2>/dev/null || stat -f %Lp "$DIR/.env")" == "600" ]] || fail ".env is not mode 0600"

step "2/10 init never overwrites without confirmation"
before="$(sha256sum "$DIR/.env" | cut -d' ' -f1)"
if og init --dir "$DIR" --yes --profile "$PROFILE" --project "$PROJECT" >/dev/null 2>&1; then
  fail "init overwrote an existing install"
fi
[[ "$before" == "$(sha256sum "$DIR/.env" | cut -d' ' -f1)" ]] || fail ".env changed"

step "3/10 runtime role cannot bypass row-level security"
rls="$(psql_owner "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = 'yupcha_runtime'")"
[[ "$rls" == "f" ]] || fail "runtime role bypasses RLS (got '$rls')"

step "3b/10 doctor passes on a healthy install"
og doctor --dir "$DIR" || fail "opengtm doctor --dir reported a problem on a healthy install"

step "4/10 write data worth keeping"
psql_owner "CREATE TABLE IF NOT EXISTS e2e_marker (id serial PRIMARY KEY, note text); INSERT INTO e2e_marker (note) VALUES ('before-backup')" >/dev/null
mkdir -p "$DIR/data"
echo "data-before-backup" > "$DIR/data/e2e-marker.txt"

step "5/10 backup, verify, restore into a scratch database, then restore in place"
og backup --dir "$DIR" --quiesce --label e2e
backups_listing="$(og backup list --dir "$DIR")"
BK="$(awk '$NF ~ /-e2e$/ {print $NF; exit}' <<<"$backups_listing")"
[[ -d "$BK" ]] || fail "no backup directory found (got '$BK')"
og backup verify "$BK"
og backup verify "$BK" --dir "$DIR" --scratch
psql_owner "INSERT INTO e2e_marker (note) VALUES ('after-backup'); DROP TABLE alembic_version" >/dev/null
echo "data-after-backup" > "$DIR/data/e2e-marker.txt"
echo "stray" > "$DIR/data/e2e-stray.txt"
if og restore --dir "$DIR" "$BK" >/dev/null 2>&1; then fail "restore into a populated database must need --wipe"; fi
og restore --dir "$DIR" --wipe --yes --start "$BK"
[[ "$(marker)" == "before-backup" ]] || fail "database not restored: '$(marker)'"
[[ "$(cat "$DIR/data/e2e-marker.txt")" == "data-before-backup" ]] || fail "data directory not restored"
[[ ! -e "$DIR/data/e2e-stray.txt" ]] || fail "stray file survived the restore"
ls -d "$DIR"/data.pre-restore-* >/dev/null 2>&1 || fail "previous data directory was not kept aside"
assert_healthy "$PREV_VERSION"

step "6/10 upgrade $PREV_VERSION -> $NEXT_VERSION"
rev_before="$(head_rev)"
og upgrade --dir "$DIR" --yes --to "$NEXT_VERSION" --app-image "$NEXT_APP_IMAGE" --server-image "$NEXT_SERVER_IMAGE" "${PULL_FLAG[@]}"
assert_healthy "$NEXT_VERSION"
[[ "$(marker)" == "before-backup" ]] || fail "data lost in the upgrade: '$(marker)'"
[[ "$(cat "$DIR/data/e2e-marker.txt")" == "data-before-backup" ]] || fail "data directory changed by the upgrade"
grep -q "^OPENGTM_VERSION=${NEXT_VERSION}$" "$DIR/.env" || fail ".env not switched to $NEXT_VERSION"
echo "schema: $rev_before -> $(head_rev)"
plan_text="$(og migrate --dir "$DIR" --plan)"
[[ "$plan_text" == *"up to date"* ]] || fail "migrate --plan does not report an up to date schema: $plan_text"

step "7/10 manual rollback restores the pre-upgrade database, data and release"
psql_owner "INSERT INTO e2e_marker (note) VALUES ('written-after-upgrade')" >/dev/null
og rollback --dir "$DIR" --yes
assert_healthy "$PREV_VERSION"
[[ "$(marker)" == "before-backup" ]] || fail "rollback did not restore the database: '$(marker)'"
[[ "$(head_rev)" == "$rev_before" ]] || fail "schema revision after rollback is $(head_rev), want $rev_before"
grep -q "^OPENGTM_VERSION=${PREV_VERSION}$" "$DIR/.env" || fail ".env not reverted"
ls -d "$DIR"/backups/*pre-rollback* >/dev/null 2>&1 || fail "no safety backup was taken before the rollback"

step "8/10 a failed upgrade rolls back automatically"
if og upgrade --dir "$DIR" --yes --auto-rollback --force --to "$NEXT_VERSION" \
     --app-image "$NEXT_APP_IMAGE" --server-image "opengtm-e2e-does-not-exist:${SUFFIX}" --no-pull; then
  fail "upgrade with an unusable server image reported success"
fi
assert_healthy "$PREV_VERSION"
[[ "$(marker)" == "before-backup" ]] || fail "failed upgrade lost data: '$(marker)'"
[[ "$(head_rev)" == "$rev_before" ]] || fail "schema after the failed upgrade is $(head_rev), want $rev_before"
grep -q "^OPENGTM_SERVER_IMAGE=${PREV_SERVER_IMAGE}$" "$DIR/.env" || fail ".env still points at the broken image"

step "9/10 the upgrade can be repeated after a rollback"
og upgrade --dir "$DIR" --yes --to "$NEXT_VERSION" --app-image "$NEXT_APP_IMAGE" --server-image "$NEXT_SERVER_IMAGE" "${PULL_FLAG[@]}"
assert_healthy "$NEXT_VERSION"
[[ "$(marker)" == "before-backup" ]] || fail "data lost after re-upgrade"

step "10/10 disaster recovery: restore a backup onto a fresh install directory"
og backup --dir "$DIR" --quiesce --label dr
backups_listing="$(og backup list --dir "$DIR")"
DRBK="$(awk '$NF ~ /-dr$/ {print $NF; exit}' <<<"$backups_listing")"
[[ -d "$DRBK" ]] || fail "no disaster-recovery backup found"
DR_PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
PY
)"
mkdir -p "$DR/data"
tar -xzf "$DRBK/config.tar.gz" -C "$DR"
# Run beside the original on another project name and port; keep every secret.
sed -i "s/^COMPOSE_PROJECT_NAME=.*/COMPOSE_PROJECT_NAME=${PROJECT}-dr/; s/^OPENGTM_PORT=.*/OPENGTM_PORT=${DR_PORT}/" "$DR/.env"
dc_dr() { docker compose --project-directory "$DR" -f "$DR/compose.yml" "$@"; }
dc_dr up -d postgres >/dev/null
og restore --dir "$DR" "$DRBK" --start
# shellcheck disable=SC2016
rec="$(dc_dr exec -T postgres sh -c 'psql -X -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT string_agg(note, chr(44) ORDER BY id) FROM e2e_marker"')"
[[ "$rec" == "before-backup" ]] || fail "recovered database lacks the data: '$rec'"
[[ "$(cat "$DR/data/e2e-marker.txt")" == "data-before-backup" ]] || fail "recovered data directory lacks the file"
[[ "$(curl -fsS "http://127.0.0.1:${DR_PORT}/api/v2/version")" == *"\"version\":\"${NEXT_VERSION}\""* ]] || fail "recovered install does not run $NEXT_VERSION"
curl -fsS "http://127.0.0.1:${DR_PORT}/readyz" >/dev/null || fail "recovered install is not ready"
[[ "$(grep ^SECRETS_MASTER_KEY= "$DR/.env")" == "$(grep ^SECRETS_MASTER_KEY= "$DIR/.env")" ]] || fail "secrets were not carried over"
dc_dr down -v --remove-orphans --timeout 5 >/dev/null

printf '\nE2E OK: profile=%s %s -> %s on %s\n' "$PROFILE" "$PREV_VERSION" "$NEXT_VERSION" "$(uname -m)"
