#!/usr/bin/env bash
# Shared helpers for the release rehearsal scripts (rehearse-install.sh, rehearse-backup-restore.sh,
# rehearse-upgrade.sh). Source it; it is not meant to be run. See docs/operations/release-rehearsals.md.
#
# Isolation: every rehearsal runs in its own Compose project (opengtm-rehearsal-<pid>), on free host ports
# chosen at start, on its own Docker network subnet, with its own generated secrets. It never reads the repo's
# .env and never touches another stack's containers, volumes or ports. The trap installed by rh_init removes
# the project, volumes included, on every exit path.
#
# Requires: docker with Compose v2.24+ (the override uses !reset), curl, python3, bash 4+.

RH_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RH_OVERRIDE="$RH_ROOT/scripts/release/rehearsal.compose.yml"
RH_FAILED=0
RH_STEPS=()
RH_SECRET_NOTE=""

if [[ -t 1 ]]; then RH_G=$'\033[32m'; RH_R=$'\033[31m'; RH_N=$'\033[0m'; else RH_G=""; RH_R=""; RH_N=""; fi

rh_info() { printf '    %s\n' "$*"; }

# rh_step "name" cmd args...: run cmd, print PASS/FAIL, record it. A failing step aborts the rehearsal
# (later steps depend on earlier ones), after printing the summary and the failing command's output.
rh_step() {
  local name="$1"; shift
  local out rc
  # Run in the current shell (steps set variables such as RH_TOKEN); capture output in a file.
  "$@" >"$RH_WORK/step.out" 2>&1; rc=$?
  out="$(cat "$RH_WORK/step.out")"
  if ((rc == 0)); then
    printf '  %sPASS%s %s\n' "$RH_G" "$RH_N" "$name"
    [[ -n "$out" ]] && printf '%s\n' "$out" | sed 's/^/         /'
    RH_STEPS+=("PASS $name")
  else
    printf '  %sFAIL%s %s\n' "$RH_R" "$RH_N" "$name"
    [[ -n "$out" ]] && printf '%s\n' "$out" | tail -n 40 | sed 's/^/         /'
    RH_STEPS+=("FAIL $name")
    RH_FAILED=1
    rh_finish
  fi
}

rh_finish() {
  echo
  echo "== ${RH_TITLE:-rehearsal} summary (project ${RH_PROJECT:-n/a}) =="
  local s
  for s in "${RH_STEPS[@]}"; do
    case "$s" in
      PASS*) printf '  %sPASS%s %s\n' "$RH_G" "$RH_N" "${s#PASS }" ;;
      *) printf '  %sFAIL%s %s\n' "$RH_R" "$RH_N" "${s#FAIL }" ;;
    esac
  done
  [[ -n "$RH_SECRET_NOTE" ]] && echo "  note: $RH_SECRET_NOTE"
  if ((RH_FAILED)); then echo "RESULT: FAIL"; exit 1; fi
  echo "RESULT: PASS"
  exit 0
}

rh_require_tools() {
  local t
  for t in docker curl python3; do
    command -v "$t" >/dev/null || { echo "rehearsal: $t is required" >&2; exit 2; }
  done
  docker compose version >/dev/null 2>&1 || { echo "rehearsal: Docker Compose v2 is required" >&2; exit 2; }
}

rh_port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# rh_free_port: a random unused TCP port in 20000-39999 on the loopback address.
rh_free_port() {
  local p tries=0
  while ((tries++ < 200)); do
    p=$((20000 + RANDOM % 20000))
    if rh_port_free "$p" && [[ " ${RH_USED_PORTS:-} " != *" $p "* ]]; then
      RH_USED_PORTS="${RH_USED_PORTS:-} $p"; echo "$p"; return 0
    fi
  done
  echo "rehearsal: no free port found" >&2; return 1
}

# rh_free_subnet: a 10.X.Y.0/24 that no Docker network uses. The root compose file pins lead_net to one subnet
# (LEAD_NET_SUBNET, default 172.29.250.0/24), so two stacks on a host would collide. 10.100-199.x is chosen on
# purpose: Docker hands the project's other, unpinned networks (browser_net, browser_egress) addresses from its
# 172.16/12 and 192.168/16 pools, and Compose creates networks in parallel, so a 172.x pin can race with them
# ("Pool overlaps with other one on this address space").
rh_free_subnet() {
  local used n m tries=0
  used="$(docker network ls -q | xargs -r docker network inspect --format '{{range .IPAM.Config}}{{.Subnet}}{{"\n"}}{{end}}' 2>/dev/null)"
  while ((tries++ < 200)); do
    n=$((100 + RANDOM % 100)); m=$((RANDOM % 256))
    if ! grep -qE "^10\.($n\.$m\.0/24|$n\.0\.0/16|0\.0\.0/8)$" <<<"$used"; then echo "10.$n.$m.0/24"; return 0; fi
  done
  echo "rehearsal: no free subnet found" >&2; return 1
}

rh_rand() { openssl rand -hex "$1" 2>/dev/null || LC_ALL=C od -An -N "$1" -tx1 /dev/urandom | tr -d ' \n'; }

# rh_init <title>: project name, work dir, ports, secrets and env file; installs the teardown trap.
# Honors RH_PROJECT_SUFFIX (default: the shell pid). The work dir lives under ${REHEARSAL_TMPDIR:-$HOME/.cache}
# because /tmp can be RAM-backed.
rh_init() {
  RH_TITLE="$1"
  rh_require_tools
  RH_PROJECT="opengtm-rehearsal-${RH_PROJECT_SUFFIX:-$$}"
  mkdir -p "${REHEARSAL_TMPDIR:-$HOME/.cache}"
  RH_WORK="$(mktemp -d "${REHEARSAL_TMPDIR:-$HOME/.cache}/opengtm-rehearsal.XXXXXX")"
  RH_ENV="$RH_WORK/env"
  RH_PORT_WEB="$(rh_free_port)"; RH_PORT_API="$(rh_free_port)"
  RH_PORT_PG="$(rh_free_port)"; RH_PORT_REDIS="$(rh_free_port)"
  RH_SUBNET="$(rh_free_subnet)"
  RH_BASE="http://127.0.0.1:$RH_PORT_WEB"
  RH_PASSWORD="rehearsal-$(rh_rand 8)-Pw"
  RH_USER="rehearsal-admin"
  umask 077
  # Same variables scripts/install.sh generates, with the same shapes; plus the rehearsal's host ports.
  cat >"$RH_ENV" <<EOF
APP_ENV=local
SECRET_KEY=$(rh_rand 48)
INTERNAL_SERVICE_TOKEN=$(rh_rand 32)
BROWSER_SERVICE_TOKEN=$(rh_rand 32)
POSTGRES_USER=yupcha
POSTGRES_PASSWORD=$(rh_rand 24)
POSTGRES_DB=yupcha
YUPCHA_RUNTIME_DB_USER=yupcha_runtime
YUPCHA_RUNTIME_DB_PASSWORD=$(rh_rand 24)
PORT=127.0.0.1:$RH_PORT_WEB
API_GO_PORT=$RH_PORT_API
POSTGRES_PORT=$RH_PORT_PG
REDIS_PORT=$RH_PORT_REDIS
LEAD_NET_SUBNET=$RH_SUBNET
EOF
  umask 022
  trap rh_teardown EXIT
  trap 'exit 130' INT TERM
  echo "== $RH_TITLE =="
  rh_info "project $RH_PROJECT; web $RH_BASE; api 127.0.0.1:$RH_PORT_API; postgres 127.0.0.1:$RH_PORT_PG; redis 127.0.0.1:$RH_PORT_REDIS; subnet $RH_SUBNET"
}

# dc: docker compose scoped to this rehearsal (project, env file, override). Always runs from the repo root so
# the relative build contexts and the nginx bind mount resolve.
dc() {
  (cd "$RH_ROOT" && docker compose -p "$RH_PROJECT" --env-file "$RH_ENV" \
    -f docker-compose.yml -f "$RH_OVERRIDE" "$@")
}

# Teardown: stops and removes this project's containers, networks and volumes (down -v is scoped by -p, so it
# cannot touch any other project). The built images are kept so the next rehearsal reuses them; set
# REHEARSAL_PRUNE_IMAGES=1 to remove them too. REHEARSAL_KEEP=1 leaves the stack up for debugging (it then
# prints the cleanup command).
rh_teardown() {
  local rc=$?
  trap - EXIT
  if [[ "${REHEARSAL_KEEP:-}" == "1" ]]; then
    echo "REHEARSAL_KEEP=1: leaving $RH_PROJECT running. Remove it with:"
    echo "  docker compose -p $RH_PROJECT down -v --remove-orphans   (env file and work dir: $RH_WORK)"
    exit "$rc"
  fi
  if ((rc != 0 && RH_FAILED == 0)); then
    echo "rehearsal aborted (exit $rc); service logs follow" >&2
    dc logs --no-color --tail 40 2>&1 | tail -n 120 >&2 || true
  fi
  [[ -n "${RH_PROJECT:-}" ]] && COMPOSE_PROFILES=demo dc down -v --remove-orphans >/dev/null 2>&1
  if [[ "${REHEARSAL_PRUNE_IMAGES:-}" == "1" ]]; then
    docker image rm -f opengtm-rehearsal-{api,worker,scheduler,migrate}-go:local opengtm-rehearsal-browser:local >/dev/null 2>&1 || true
  fi
  [[ -n "${RH_WORK:-}" ]] && rm -rf "$RH_WORK"
  # Confirm nothing of ours is left.
  local left
  left="$(docker ps -aq --filter "label=com.docker.compose.project=$RH_PROJECT" 2>/dev/null | wc -l)"
  left=$((left + $(docker volume ls -q --filter "label=com.docker.compose.project=$RH_PROJECT" 2>/dev/null | wc -l)))
  echo "teardown: project $RH_PROJECT removed (containers and volumes left: $left)"
  exit "$rc"
}

# --- stack lifecycle -------------------------------------------------------------------------------------

# rh_build: build (or reuse cached layers of) the stack images. REHEARSAL_NO_BUILD=1 requires them to exist.
rh_build() {
  if [[ "${REHEARSAL_NO_BUILD:-}" == "1" ]]; then
    local i
    for i in api worker scheduler migrate; do
      docker image inspect "opengtm-rehearsal-$i-go:local" >/dev/null 2>&1 || { echo "REHEARSAL_NO_BUILD=1 but opengtm-rehearsal-$i-go:local is missing"; return 1; }
    done
    docker image inspect opengtm-rehearsal-browser:local >/dev/null 2>&1 || { echo "REHEARSAL_NO_BUILD=1 but opengtm-rehearsal-browser:local is missing"; return 1; }
    echo "reusing existing images"
    return 0
  fi
  dc build 2>&1 | tail -n 5
  return "${PIPESTATUS[0]}"
}

# rh_up [services...]: start the stack (default services only; the demo profile is used by rh_seed_demo).
rh_up() { dc up -d "$@" 2>&1 | tail -n 20; return "${PIPESTATUS[0]}"; }

# rh_wait <timeout-seconds> <desc> cmd...: poll a command until it succeeds.
rh_wait() {
  local timeout="$1" desc="$2"; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then echo "timed out after ${timeout}s waiting for: $desc"; return 1; fi
    sleep 2
  done
}

rh_http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$@"; }
rh_healthz_ok() { [[ "$(rh_http_code "$RH_BASE/healthz")" == "200" ]]; }

# rh_wait_healthy: wait for every long-running service with a healthcheck to be healthy, and for /healthz
# through nginx. Prints the service table on failure.
rh_wait_healthy() {
  rh_wait "${REHEARSAL_HEALTH_TIMEOUT:-240}" "/healthz through nginx at $RH_BASE" rh_healthz_ok || { dc ps -a 2>&1; dc logs --no-color --tail 30 migrate api-go 2>&1; return 1; }
  local bad
  bad="$(dc ps -a --format '{{.Service}}|{{.State}}|{{.Health}}|{{.ExitCode}}' 2>&1 | awk -F'|' '
    $1=="migrate" { if ($2!="exited" || $4!="0") print; next }
    $2!="running" { print; next }
    $3=="unhealthy" { print }')"
  if [[ -n "$bad" ]]; then echo "unhealthy or stopped services:"; echo "$bad"; dc ps -a; return 1; fi
  echo "migrate exited 0; api-go, worker-go, scheduler-go, browser, nginx, postgres, redis running"
}

# --- API helpers (through nginx, like a browser) ---------------------------------------------------------

# rh_json <python-expr on d>: read JSON on stdin and print the expression (d is the parsed document).
# The expression is always a literal written in these scripts, never data from the server; eval is used only
# to keep the call sites short.
rh_json() { python3 -I -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))' "$1"; }

# rh_setup_token: the one-time setup token from the api-go log (what the README tells a user to do).
rh_setup_token() {
  dc logs --no-color api-go 2>&1 | grep -o '"setup_token":"[^"]*"' | head -n1 | sed 's/.*"setup_token":"\([^"]*\)"/\1/'
}

rh_setup_status() { curl -s --max-time 10 "$RH_BASE/auth/setup/status"; }

# rh_first_run_setup: POST /auth/setup with the logged token (the UI's /setup page does exactly this).
rh_first_run_setup() {
  local status tok code body
  status="$(rh_setup_status)"
  [[ "$(echo "$status" | rh_json "d['setup_required']")" == "True" ]] || { echo "setup is not required on a clean install: $status"; return 1; }
  tok="$(rh_setup_token)"
  if [[ "$(echo "$status" | rh_json "d['token_required']")" == "True" && -z "$tok" ]]; then
    echo "setup requires a token but none was found in the api-go log"; return 1
  fi
  body="$(python3 -I -c 'import json,sys; print(json.dumps({"username":sys.argv[1],"password":sys.argv[2],"workspace_name":"Rehearsal","setup_token":sys.argv[3]}))' "$RH_USER" "$RH_PASSWORD" "$tok")"
  code="$(curl -s -o "$RH_WORK/setup.json" -w '%{http_code}' --max-time 30 -H 'Content-Type: application/json' -d "$body" "$RH_BASE/auth/setup")"
  [[ "$code" == "200" ]] || { echo "POST /auth/setup -> HTTP $code: $(cat "$RH_WORK/setup.json")"; return 1; }
  echo "POST /auth/setup -> 200 (admin '$RH_USER' created with the token from the api-go log)"
  [[ "$(rh_setup_status | rh_json "d['setup_required']")" == "False" ]] || { echo "setup/status still reports setup_required"; return 1; }
}

# rh_login: POST /auth/token (form login) -> RH_TOKEN; then GET /auth/me.
rh_login() {
  local code
  code="$(curl -s -o "$RH_WORK/token.json" -w '%{http_code}' --max-time 30 \
    --data-urlencode "username=$RH_USER" --data-urlencode "password=$RH_PASSWORD" "$RH_BASE/auth/token")"
  [[ "$code" == "200" ]] || { echo "POST /auth/token -> HTTP $code: $(cat "$RH_WORK/token.json")"; return 1; }
  RH_TOKEN="$(rh_json "d['access_token']" <"$RH_WORK/token.json")"
  [[ -n "$RH_TOKEN" ]] || { echo "no access_token in the login response"; return 1; }
  local me; me="$(curl -s --max-time 10 -H "Authorization: Bearer $RH_TOKEN" "$RH_BASE/auth/me")"
  [[ "$(echo "$me" | rh_json "d['username']")" == "$RH_USER" ]] || { echo "GET /auth/me did not return $RH_USER: $me"; return 1; }
  echo "logged in as $RH_USER; GET /auth/me ok"
}

# rh_api <path> [curl args]: authenticated GET; prints the body, fails on a non-2xx status.
rh_api() {
  local path="$1"; shift
  local code
  code="$(curl -s -o "$RH_WORK/api.out" -w '%{http_code}' --max-time 30 -H "Authorization: Bearer $RH_TOKEN" "$@" "$RH_BASE$path")"
  cat "$RH_WORK/api.out"
  [[ "$code" =~ ^2 ]] || { echo; echo "HTTP $code for $path" >&2; return 1; }
}

# rh_seed_demo: run the demo seed (compose `demo` profile) against the workspace setup created. It seeds the
# workbook only (the override sets SEED_START_RUN=0).
rh_seed_demo() { dc --profile demo run --rm seed-go 2>&1 | tail -n 5; return "${PIPESTATUS[0]}"; }

# rh_psql <sql>: run SQL as the database owner inside this project's postgres container; prints tuples only.
rh_psql() { dc exec -T postgres psql -U yupcha -d yupcha -v ON_ERROR_STOP=1 -At -c "$1"; }

# Smoke checks shared by the rehearsals --------------------------------------------------------------------

rh_check_healthz() {
  local body; body="$(curl -s --max-time 10 "$RH_BASE/healthz")"
  [[ "$(rh_http_code "$RH_BASE/healthz")" == "200" ]] || { echo "/healthz is not 200: $body"; return 1; }
  echo "GET /healthz -> 200 $body"
}

rh_check_ui() {
  local code body
  code="$(curl -s -o "$RH_WORK/index.html" -w '%{http_code}' --max-time 10 "$RH_BASE/")"
  [[ "$code" == "200" ]] || { echo "GET / -> HTTP $code"; return 1; }
  grep -qi '<div id="root"' "$RH_WORK/index.html" || grep -qi '<html' "$RH_WORK/index.html" || { echo "GET / did not return an HTML document"; return 1; }
  # The built bundle must be served too, not only the shell.
  local asset; asset="$(grep -o 'src="/assets/[^"]*\.js"' "$RH_WORK/index.html" | head -n1 | sed 's/src="\(.*\)"/\1/')"
  [[ -n "$asset" ]] || { echo "index.html references no /assets/*.js bundle"; return 1; }
  code="$(rh_http_code "$RH_BASE$asset")"
  [[ "$code" == "200" ]] || { echo "GET $asset -> HTTP $code"; return 1; }
  echo "GET / -> 200 (HTML), GET $asset -> 200"
}

# rh_check_demo_workbook: the seeded demo workbook is listed, and its detail reads back with all 25 demo rows
# and 10 columns.
rh_check_demo_workbook() {
  local list detail
  list="$(rh_api /api/workbooks/)" || return 1
  [[ "$(echo "$list" | rh_json "any(w.get('id')=='demo-zero-key-firstrun' for w in d.get('workbooks',[]))")" == "True" ]] \
    || { echo "demo workbook is not in GET /api/workbooks/: $(echo "$list" | head -c 400)"; return 1; }
  detail="$(rh_api /api/workbooks/demo-zero-key-firstrun)" || return 1
  [[ "$(echo "$detail" | rh_json "'%s|%d|%d|%d' % (d['workbook']['name'], len(d['rows']), d['total_rows'], len(d['workbook']['columns_config']))")" \
    == "Demo — Zero-Key Enrichment|25|25|10" ]] \
    || { echo "unexpected demo workbook detail: $(echo "$detail" | head -c 300)"; return 1; }
  echo "GET /api/workbooks/ lists demo-zero-key-firstrun; its detail returns 25 rows and 10 columns"
}

