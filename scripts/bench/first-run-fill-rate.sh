#!/usr/bin/env bash
# First-run fill rate: on a clean install of the Go runtime with NO paid keys and NO LLM key, what share of the
# demo workbook's enrichment cells fill, per column, and why does each unfilled cell stay empty?
#
# End to end: generate an env file and a compose override OUTSIDE the repo, build and start the stack under its
# own compose project, complete first-run setup through the API, seed and run the demo (compose "demo" profile),
# wait for the run to settle, read workbook_enrichments, print the per-column table and three separate fill rates
# (all filled; excluding guessed emails; excluding columns that need a key by design) as text and JSON, then tear the project down (containers, volumes, network, built images) in a trap.
#
# Requires: docker compose v2.24+ (for !reset), curl, python3, openssl. Makes real outbound requests to free
# public sources (that is the point). Run from anywhere; it works on the checkout it lives in.
#
# Env knobs (all optional):
#   FILLRATE_PROJECT       compose project name (default opengtm-fillrate)
#   FILLRATE_RERUNS        number of forced re-runs after the first run, 0..2 (default 0)
#   FILLRATE_KEEP          1 = do not tear down (for debugging; you must run `docker compose -p ... down -v`)
#   FILLRATE_KEEP_IMAGES   1 = keep the built images (default: remove them in teardown)
#   FILLRATE_RUN_TIMEOUT   seconds to wait for a run to settle (default 900)
#   FILLRATE_OUT           directory for work files and report.json (default: a fresh mktemp dir)
#   FILLRATE_PORT, FILLRATE_API_PORT, FILLRATE_POSTGRES_PORT, FILLRATE_REDIS_PORT   host ports (default: free ports)
#   FILLRATE_SUBNET        lead_net subnet (default: first free 172.29.X.0/24 from 251 up)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROJECT="${FILLRATE_PROJECT:-opengtm-fillrate}"
RERUNS="${FILLRATE_RERUNS:-0}"
RUN_TIMEOUT="${FILLRATE_RUN_TIMEOUT:-900}"
OUT="${FILLRATE_OUT:-$(mktemp -d "${TMPDIR:-/tmp}/fillrate.XXXXXX")}"
mkdir -p "$OUT"
ENVF="$OUT/env"
OVR="$OUT/override.yml"
IMG="$PROJECT"

log() { printf '[fillrate %s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
dc() { docker compose -p "$PROJECT" --env-file "$ENVF" -f "$ROOT/docker-compose.yml" -f "$OVR" --profile demo "$@"; }

STATS_PID=""
teardown() {
  local rc=$?
  [ -n "$STATS_PID" ] && kill "$STATS_PID" 2>/dev/null || true
  if [ "${FILLRATE_KEEP:-0}" = 1 ]; then
    log "FILLRATE_KEEP=1: leaving project $PROJECT running; work files in $OUT"
    exit "$rc"
  fi
  log "tearing down project $PROJECT (containers, volumes, network)"
  [ -f "$ENVF" ] && dc down -v --remove-orphans >/dev/null 2>&1 || true
  if [ "${FILLRATE_KEEP_IMAGES:-0}" != 1 ]; then
    docker rmi -f "$IMG-api:local" "$IMG-worker:local" "$IMG-sched:local" "$IMG-migrate:local" "$IMG-browser:local" >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap teardown EXIT INT TERM

for bin in docker curl python3 openssl; do command -v "$bin" >/dev/null || { echo "missing: $bin" >&2; exit 2; }; done

port_free() { ! (ss -ltn 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]$1$"); }
free_port() { local p="$1"; while ! port_free "$p"; do p=$((p + 1)); done; echo "$p"; }
free_subnet() {
  local used n
  used="$(docker network inspect $(docker network ls -q) --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null)"
  for n in $(seq 251 254); do
    echo "$used" | grep -q "172.29.$n.0/24" || { echo "172.29.$n.0/24"; return; }
  done
  echo "no free 172.29.X.0/24 subnet; set FILLRATE_SUBNET" >&2; exit 2
}

PORT="${FILLRATE_PORT:-$(free_port 3190)}"
API_PORT="${FILLRATE_API_PORT:-$(free_port 8191)}"
PG_PORT="${FILLRATE_POSTGRES_PORT:-$(free_port 5439)}"
REDIS_PORT="${FILLRATE_REDIS_PORT:-$(free_port 6389)}"
SUBNET="${FILLRATE_SUBNET:-$(free_subnet)}"
rnd() { openssl rand -hex 24; }

# No provider keys, no LLM key: only the secrets the stack refuses to start without.
cat >"$ENVF" <<EOF
APP_ENV=dev
SECRET_KEY=$(rnd)
INTERNAL_SERVICE_TOKEN=$(rnd)
BROWSER_SERVICE_TOKEN=$(rnd)
POSTGRES_PASSWORD=$(rnd)
YUPCHA_RUNTIME_DB_PASSWORD=$(rnd)
PORT=$PORT
API_GO_PORT=$API_PORT
POSTGRES_PORT=$PG_PORT
REDIS_PORT=$REDIS_PORT
LEAD_NET_SUBNET=$SUBNET
EOF
chmod 600 "$ENVF"

# Collisions with another stack of this repo on the same host, worked around here (see the benchmark doc):
#  - fixed container_name on postgres, redis, nginx (and reacher)
#  - fixed `image:` tags (opengtm-*-go:local) that a build would retag under a running stack
#  - fixed lead_net subnet 172.29.250.0/24 (overridden through LEAD_NET_SUBNET above)
cat >"$OVR" <<EOF
services:
  postgres: {container_name: !reset null}
  redis: {container_name: !reset null}
  nginx: {container_name: !reset null}
  reacher: {container_name: !reset null}
  migrate: {image: $IMG-migrate:local}
  api-go: {image: $IMG-api:local}
  worker-go: {image: $IMG-worker:local}
  scheduler-go: {image: $IMG-sched:local}
  seed-go: {image: $IMG-sched:local}
  browser: {image: $IMG-browser:local}
EOF

COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
log "commit $COMMIT, project $PROJECT, ports web=$PORT api=$API_PORT pg=$PG_PORT redis=$REDIS_PORT, subnet $SUBNET, out $OUT"
df -h / | tail -1 >&2
free -m | sed -n 2p >&2

pg() { docker exec -i "$(dc ps -q postgres)" psql -U yupcha -d yupcha -tA "$@"; }
now() { date +%s; }

T_START="$(now)"
log "building images (a cold build downloads Go/Bun/Playwright layers: minutes)"
COMPOSE_PARALLEL_LIMIT=1 dc build >"$OUT/build.log" 2>&1 || { tail -30 "$OUT/build.log" >&2; exit 1; }
T_BUILT="$(now)"

log "docker compose up"
T_UP="$(now)"
dc up -d >"$OUT/up.log" 2>&1 || { tail -30 "$OUT/up.log" >&2; exit 1; }
T_UP_DONE="$(now)"

# Peak RAM sampler (docker stats, every 10 s, whole project).
(
  while :; do
    docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' $(docker ps -q --filter "label=com.docker.compose.project=$PROJECT") >>"$OUT/stats.log" 2>/dev/null || true
    echo '---' >>"$OUT/stats.log"
    sleep 10
  done
) &
STATS_PID=$!

API="http://127.0.0.1:$API_PORT"
for _ in $(seq 60); do curl -fsS "$API/auth/setup/status" >/dev/null 2>&1 && break; sleep 2; done
log "first-run setup through the API"
TOKEN=""
for _ in $(seq 30); do
  TOKEN="$(dc logs api-go 2>&1 | grep -o '"setup_token":"[^"]*"' | head -1 | cut -d'"' -f4 || true)"
  [ -n "$TOKEN" ] && break; sleep 2
done
[ -n "$TOKEN" ] || { echo "no setup token in the api-go log" >&2; exit 1; }
curl -fsS -X POST "$API/auth/setup" -H 'content-type: application/json' \
  -d "{\"username\":\"fillrate\",\"password\":\"$(rnd)\",\"workspace_name\":\"main\",\"setup_token\":\"$TOKEN\"}" >/dev/null \
  || { echo "setup failed" >&2; exit 1; }

# seed-go was started by `up` (demo profile): it waits for setup, seeds 25 rows and starts the run through the API.
log "waiting for the seed"
for _ in $(seq 60); do
  dc logs seed-go 2>&1 | grep -q '"seed complete"' && break; sleep 2
done
dc logs seed-go 2>&1 | grep '"seed complete"' | grep -q '"run_started":true' || { dc logs seed-go >&2; echo "seed did not start the run" >&2; exit 1; }
T_RUN_START="$(now)"

wait_run() { # $1 = number of run_workbook jobs expected so far
  local deadline=$(($(now) + RUN_TIMEOUT)) n s
  while [ "$(now)" -lt "$deadline" ]; do
    n="$(pg -c "select count(*) from jobs where type='run_workbook'")"
    s="$(pg -c "select status from jobs where type='run_workbook' order by id desc limit 1")"
    if [ "$n" -ge "$1" ] && [ "$s" != processing ] && [ "$s" != pending ]; then echo "$s"; return 0; fi
    sleep 5
  done
  echo timeout; return 1
}

STATUS1="$(wait_run 1)" || true
T_RUN_DONE="$(now)"
log "run 1 status: $STATUS1 in $((T_RUN_DONE - T_RUN_START)) s"
pg -c "select column_id,lead_id,regexp_replace(replace(coalesce(value,''),'|','/'),'[\r\n]+',' ','g'),coalesce(provider,''),coalesce(status,''),regexp_replace(replace(coalesce(error,''),'|','/'),'[\r\n]+',' ','g'),regexp_replace(replace(coalesce(cell_metadata::text,''),'|','/'),'[\r\n]+',' ','g') from workbook_enrichments order by 1,2" >"$OUT/run1_cells.psv"
dc logs worker-go >"$OUT/worker.log" 2>&1 || true
dc logs browser >"$OUT/browser.log" 2>&1 || true

# Optional re-runs (forced through the seed). Run 1 is the headline number.
RERUN_JSON="[]"
for i in $(seq 1 "$RERUNS"); do
  [ "$i" -le 2 ] || break
  log "forced re-run $i"
  t0="$(now)"
  SEED_FORCE_RUN=1 dc run --rm seed-go >>"$OUT/seed-rerun.log" 2>&1 || true
  st="$(wait_run $((i + 1)))" || true
  pg -c "select column_id,lead_id,regexp_replace(replace(coalesce(value,''),'|','/'),'[\r\n]+',' ','g'),coalesce(provider,''),coalesce(status,''),regexp_replace(replace(coalesce(error,''),'|','/'),'[\r\n]+',' ','g'),regexp_replace(replace(coalesce(cell_metadata::text,''),'|','/'),'[\r\n]+',' ','g') from workbook_enrichments order by 1,2" >"$OUT/run$((i + 1))_cells.psv"
  RERUN_JSON="$(python3 -I - "$OUT" "$i" "$st" "$(($(now) - t0))" "$RERUN_JSON" <<'PY'
import json, sys
out, i, st, secs, prev = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4]), json.loads(sys.argv[5])
def load(p):
    d = {}
    for l in open(p):
        f = l.rstrip("\n").split("|")
        if len(f) >= 3: d[(f[0], f[1])] = f
    return d
a, b = load(f"{out}/run{i}_cells.psv"), load(f"{out}/run{i+1}_cells.psv")
prev.append({"run": i + 1, "status": st, "seconds": secs,
             "filled": sum(1 for r in b.values() if r[2]), "total": len(b),
             "lost_vs_previous": sum(1 for k, r in a.items() if r[2] and not b.get(k, [0, 0, ""])[2]),
             "gained_vs_previous": sum(1 for k, r in b.items() if r[2] and not a.get(k, [0, 0, ""])[2])})
print(json.dumps(prev))
PY
)"
done

log "measuring"
ROWS="$(pg -c "select count(*) from workbook_rows")"
LEADFIELD_FILLED="$(pg -c "select count(*) filter (where coalesce(data->>'company','')<>'')+count(*) filter (where coalesce(data->>'website','')<>'') from workbook_rows")"
pg -c "select provider,field,attempts,hits,coalesce(cooldown_until::text,'') from provider_stats order by 1,2" >"$OUT/provider_stats.psv" || true

pg -c "select columns_config::text from workbooks limit 1" >"$OUT/columns.json" || true
python3 -I "$ROOT/scripts/bench/fill_rate_report.py" "$OUT" "$COMMIT" "$ROWS" "$LEADFIELD_FILLED" "$STATUS1" "$((T_UP_DONE - T_UP))" "$((T_RUN_DONE - T_UP))" \
  "$((T_RUN_DONE - T_RUN_START))" "$((T_BUILT - T_START))" "$RERUN_JSON" | tee "$OUT/report.txt"
log "report: $OUT/report.txt and $OUT/report.json (kept after teardown)"
