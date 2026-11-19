#!/usr/bin/env bash
# First-run fill rate: on a clean install of the Go runtime with NO paid keys and NO LLM key, what share of the
# demo workbook's enrichment cells fill, per column, and why does each unfilled cell stay empty?
#
# End to end: generate an env file and a compose override OUTSIDE the repo, build and start the stack under its
# own compose project, complete first-run setup through the API, seed and run the demo (compose "demo" profile),
# wait for the run to settle, read workbook_enrichments, print the per-column table and the overall rate as text
# and JSON, then tear the project down (containers, volumes, network, built images) in a trap.
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
pg -c "select column_id,lead_id,coalesce(value,''),coalesce(provider,''),coalesce(status,''),coalesce(error,''),coalesce(cell_metadata::text,'') from workbook_enrichments order by 1,2" >"$OUT/run1_cells.psv"
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
  pg -c "select column_id,lead_id,coalesce(value,''),coalesce(provider,''),coalesce(status,''),coalesce(error,''),coalesce(cell_metadata::text,'') from workbook_enrichments order by 1,2" >"$OUT/run$((i + 1))_cells.psv"
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

python3 -I - "$OUT" "$COMMIT" "$ROWS" "$LEADFIELD_FILLED" "$STATUS1" "$((T_UP_DONE - T_UP))" "$((T_RUN_DONE - T_UP))" \
  "$((T_RUN_DONE - T_RUN_START))" "$((T_BUILT - T_START))" "$RERUN_JSON" <<'PY' | tee "$OUT/report.txt"
import collections, json, re, sys
out, commit, rows, lf_filled, status, up_s, up_to_done_s, run_s, build_s, reruns = sys.argv[1:11]
rows, lf_filled = int(rows), int(lf_filled)
cells = []
for l in open(f"{out}/run1_cells.psv"):
    f = l.rstrip("\n").split("|", 6)
    if len(f) == 7: cells.append(f)
cols = collections.OrderedDict()
for col, lead, val, prov, st, err, meta in cells:
    c = cols.setdefault(col, {"total": 0, "filled": 0, "providers": collections.Counter(), "empty_reasons": collections.Counter()})
    c["total"] += 1
    if val:
        c["filled"] += 1; c["providers"][prov or "?"] += 1
    else:
        reason = err or st or "unknown"
        if "cooldown" in meta: reason += " (provider cooldown skip)"
        c["empty_reasons"][reason] += 1
tot = sum(c["total"] for c in cols.values()); fil = sum(c["filled"] for c in cols.values())

def mib(s):
    n, u = re.match(r"([\d.]+)(\w+)", s).groups()
    return float(n) * {"KiB": 1/1024, "MiB": 1, "GiB": 1024, "B": 1/1048576}[u]
peak, per, cur = 0.0, collections.defaultdict(float), 0.0
try:
    for l in open(f"{out}/stats.log"):
        if l.startswith("---"): peak = max(peak, cur); cur = 0; continue
        p = l.split()
        if len(p) >= 2:
            m = mib(p[1]); per[p[0]] = max(per[p[0]], m); cur += m
except FileNotFoundError:
    pass
pstats = []
try:
    for l in open(f"{out}/provider_stats.psv"):
        f = l.rstrip("\n").split("|")
        if len(f) >= 4: pstats.append({"provider": f[0], "field": f[1], "attempts": int(f[2]), "hits": int(f[3])})
except FileNotFoundError:
    pass
rep = {
    "commit": commit, "rows": rows, "run_status": status,
    "enrichment_cells": {"filled": fil, "total": tot, "rate": round(fil / tot, 4) if tot else None},
    "all_cells_including_lead_fields": {"filled": fil + lf_filled, "total": tot + 2 * rows,
                                         "rate": round((fil + lf_filled) / (tot + 2 * rows), 4) if tot else None},
    "per_column": {k: {"filled": v["filled"], "total": v["total"],
                       "rate": round(v["filled"] / v["total"], 4),
                       "filled_by_provider": dict(v["providers"]),
                       "empty_reasons": dict(v["empty_reasons"])} for k, v in cols.items()},
    "seconds": {"build": int(build_s), "compose_up": int(up_s), "up_to_run_complete": int(up_to_done_s), "run_1": int(run_s)},
    "memory_mib": {"peak_sum_sampled": round(peak), "peak_per_container": {k: round(v) for k, v in per.items()},
                   "samples": "docker stats --no-stream every 10 s"},
    "provider_stats_cumulative": pstats,
    "reruns": json.loads(reruns),
}
print(f"First-run fill rate, commit {commit[:8]}, {rows} rows, no paid keys, no LLM key, run status {status}")
print(f"{'column':<14}{'filled':>8}{'total':>7}{'rate':>7}  filled by / empty because")
for k, v in rep["per_column"].items():
    fb = ", ".join(f"{p} {n}" for p, n in sorted(v["filled_by_provider"].items(), key=lambda x: -x[1]))
    eb = ", ".join(f"{r} x{n}" for r, n in sorted(v["empty_reasons"].items(), key=lambda x: -x[1]))
    print(f"{k:<14}{v['filled']:>8}{v['total']:>7}{v['rate']*100:>6.0f}%  {fb}{' | EMPTY: ' + eb if eb else ''}")
e = rep["enrichment_cells"]
print(f"OVERALL (enrichment columns): {e['filled']}/{e['total']} = {e['rate']*100:.1f}%")
a = rep["all_cells_including_lead_fields"]
print(f"OVERALL (incl. the 2 lead-field columns): {a['filled']}/{a['total']} = {a['rate']*100:.1f}%")
s = rep["seconds"]
print(f"time: compose up {s['compose_up']} s, up to run complete {s['up_to_run_complete']} s, run 1 {s['run_1']} s (image build {s['build']} s, separate)")
print(f"memory: peak sum of sampled containers {rep['memory_mib']['peak_sum_sampled']} MiB; per container {rep['memory_mib']['peak_per_container']}")
for r in rep["reruns"]:
    print(f"re-run {r['run']}: {r['filled']}/{r['total']} filled, lost {r['lost_vs_previous']}, gained {r['gained_vs_previous']} vs previous run, {r['seconds']} s")
print("JSON:")
print(json.dumps(rep, indent=2))
json.dump(rep, open(f"{out}/report.json", "w"), indent=2)
PY
log "report: $OUT/report.txt and $OUT/report.json (kept after teardown)"
