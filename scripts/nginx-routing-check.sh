#!/usr/bin/env bash
# Routing check for the side-by-side Go/Python API (plan 02, Task 15).
# Runs a throwaway nginx (pinned image) with the REAL infra/nginx config and two stub
# upstreams aliased `api` (py) and `api-go` (go) on a private docker network.
# Asserts `nginx -t`, then the curl matrix at 0% and 100%.
#   usage: scripts/nginx-routing-check.sh        env: NGINX_IMAGE (default nginx:1.27-alpine)
set -euo pipefail
cd "$(dirname "$0")/.."
IMG="${NGINX_IMAGE:-nginx:1.27-alpine}"
RUN="ngx-route-check-$$"
TMP="$(mktemp -d)"
FAIL=0
cleanup() { docker rm -f "$RUN-py" "$RUN-go" "$RUN-gw" >/dev/null 2>&1 || true
            docker network rm "$RUN" >/dev/null 2>&1 || true; rm -rf "$TMP"; }
trap cleanup EXIT

stub_conf() { # $1 = name
  cat > "$TMP/stub-$1.conf" <<CONF
server { listen 8000; location / {
  add_header X-Stub $1 always; add_header X-Seen-Uri \$request_uri always;
  add_header X-Seen-Proto \$server_protocol always; add_header X-Seen-Conn \$http_connection always;
  add_header X-Seen-Upgrade \$http_upgrade always;
  return 200 "$1\n"; } }
CONF
}
docker network create "$RUN" >/dev/null
for n in py go; do
  stub_conf "$n"
  alias=api; [ "$n" = go ] && alias=api-go
  docker run -d --name "$RUN-$n" --network "$RUN" --network-alias "$alias" \
    -v "$TMP/stub-$n.conf:/etc/nginx/conf.d/default.conf:ro" "$IMG" >/dev/null
done

start_gateway() { # $1 = percentage for the go bucket
  docker rm -f "$RUN-gw" >/dev/null 2>&1 || true
  if [ "$1" = 0 ]; then cp infra/nginx/go-split.conf "$TMP/go-split.conf"   # shipped state = 0%
  else sed -E "s/^[[:space:]]*#[[:space:]]*[0-9.]+%[[:space:]]+go;.*/    $1% go;/" infra/nginx/go-split.conf > "$TMP/go-split.conf"; fi
  docker run -d --name "$RUN-gw" --network "$RUN" -p 127.0.0.1::80 \
    -v "$PWD/infra/nginx/default.conf:/etc/nginx/conf.d/default.conf:ro" \
    -v "$PWD/infra/nginx/go-routes.conf:/etc/nginx/go-routes.conf:ro" \
    -v "$TMP/go-split.conf:/etc/nginx/go-split.conf:ro" "$IMG" >/dev/null
  sleep 0.5; PORT="$(docker port "$RUN-gw" 80/tcp 2>/dev/null | head -1 | sed "s/.*://")"
  [ -n "$PORT" ] || { docker logs "$RUN-gw" 2>&1 | tail -8; return 1; }
  for _ in $(seq 40); do curl -s -o /dev/null "http://127.0.0.1:$PORT/health" && return 0; sleep 0.25; done
  docker logs "$RUN-gw" 2>&1 | tail -5; return 1
}
hdr() { curl -s -D- -o /dev/null -X "$1" "http://127.0.0.1:$PORT$2" | tr -d '\r' | awk -F': ' -v k="$3" 'tolower($1)==tolower(k){print $2}'; }
code() { curl -s -o /dev/null -w '%{http_code}' -X "$1" "http://127.0.0.1:$PORT$2"; }
expect() { # desc got want
  if [ "$2" = "$3" ]; then echo "  ok   $1 = $2"; else echo "  FAIL $1: got '$2' want '$3'"; FAIL=1; fi; }

check_matrix() { # $1 = expected upstream for Go-eligible routes (py|go)
  local want="$1"
  echo "== bucket $2 =="
  for r in "POST /auth/token" "POST /auth/refresh" "GET /auth/me" "GET /api/workspaces" \
           "GET /api/workspaces/1/members" "GET /api/mcp/tokens" "DELETE /api/mcp/tokens/3" \
           "GET /api/settings/providers" "PUT /api/settings/providers/groq" "POST /api/settings/providers/groq/test" \
           "GET /api/settings/enrichment-perf" "PUT /api/settings/enrichment-perf" "GET /api/settings/system" \
           "GET /api/settings/models/anthropic" "GET /api/settings/enrichment-providers" \
           "PUT /api/settings/enrichment-providers/hunter_io" "GET /api/settings/llm-usage" \
           "GET /api/settings/integrations" "PUT /api/settings/integrations/salesforce" \
           "GET /api/settings/workspace-integrations" "PUT /api/settings/workspace-integrations/hubspot" \
           "GET /api/billing/balance" "POST /api/billing/checkout" "POST /api/billing/webhook" \
           "GET /api/leads" "GET /api/leads?limit=5" "GET /api/leads/" "GET /api/stats" "GET /api/filters" "POST /api/lead" \
           "GET /api/lead/7" "PUT /api/lead/7" "DELETE /api/lead/7" "GET /api/lead/7/timeline" "GET /api/lead/7/similar" \
           "POST /api/lead/7/status" "GET /api/export/csv" "POST /api/leads/dedup" "POST /api/leads/dedup/merge" \
           "POST /api/leads/bulk-enrich" "GET /api/signals" "GET /api/signals/analytics" "GET /api/signals/counts" \
           "POST /api/signals/scan" "POST /api/signals/mark-read" "GET /api/events?token=SECRET"; do
    set -- $r
    expect "$r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" "$want"
    expect "$r X-Stub     " "$(hdr "$1" "$2" X-Stub)" "$want"
    expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Proxy settings must match the location each route would otherwise fall into.
  for r in "GET /api/workspaces" "GET /api/mcp/tokens" "GET /api/settings/providers" "GET /api/settings/system" \
           "GET /api/settings/llm-usage" "POST /api/billing/webhook" "GET /api/leads"; do
    set -- $r
    expect "$2 upstream HTTP version" "$(curl -s -D- -o /dev/null -H 'Upgrade: websocket' "http://127.0.0.1:$PORT$2" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-seen-proto"{print $2}')" "HTTP/1.1"
    expect "$2 Connection to upstream" "$(curl -s -D- -o /dev/null -H 'Upgrade: websocket' "http://127.0.0.1:$PORT$2" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-seen-conn"{print $2}')" upgrade
    expect "$2 Upgrade to upstream" "$(curl -s -D- -o /dev/null -H 'Upgrade: websocket' "http://127.0.0.1:$PORT$2" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-seen-upgrade"{print $2}')" websocket
  done
  # Timeouts/buffering are not observable from a stub: compare each location block in `nginx -T`.
  local T; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  for l in "^~ /api/workspaces" "^~ /api/mcp/tokens" "^~ /api/settings/providers" "^~ /api/settings/enrichment-perf" \
           "= /api/settings/system" "^~ /api/settings/models/" "^~ /api/settings/enrichment-providers" \
           "= /api/settings/llm-usage" "^~ /api/settings/integrations" "^~ /api/settings/workspace-integrations" \
           "^~ /api/billing/"; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
  # Always Python, at any percentage
  expect "GET /auth/sso/x X-Stub" "$(hdr GET /auth/sso/x X-Stub)" py
  expect "GET /auth/sso/x X-Upstream (unset)" "$(hdr GET /auth/sso/x X-Upstream)" ""
  # Retained lead/signal routes (testdata/contract/retained_leads_routes.txt): always Python at any percentage.
  for r in "POST /api/lead/7/enrich" "POST /api/collect" "GET /api/jobs" "GET /api/jobs/x" "GET /api/jobs/x/stages" \
           "GET /api/jobs/x/leads" "GET /api/jobs/x/events" "POST /api/jobs/x/cancel" "DELETE /api/jobs/x" \
           "POST /api/jobs/x/retry" "GET /api/system-stats" "GET /api/v2/search/unified" "POST /api/v2/scraper/scrape" \
           "POST /api/leads/verify-email" "POST /api/leads/score" "POST /api/leads/tech-stack" \
           "POST /api/leads/domain-intel" "GET /api/signals/sources"; do
    set -- $r
    expect "$r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "$r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # /api/events: SSE settings and no `?token=` in this location's access log.
  local E; E="$(printf '%s\n' "$T" | awk 'index($0,"location = /api/events {"){f=1;next} f&&/^[[:space:]]*}/{exit} f')"
  for d in "proxy_buffering off;" "proxy_read_timeout 1h;" "proxy_http_version 1.1;" "add_header X-Accel-Buffering no always;" "access_log /dev/stdout sse_noquery;" "error_log /dev/stderr crit;" "limit_conn sse_per_ip 20;" "limit_conn_status 429;"; do
    case "$E" in *"$d"*) echo "  ok   /api/events has '$d'";; *) echo "  FAIL /api/events missing '$d'"; FAIL=1;; esac
  done
  expect "GET /api/events X-Accel-Buffering" "$(hdr GET '/api/events?token=SECRET' X-Accel-Buffering)" no
  curl -s -o /dev/null "http://127.0.0.1:$PORT/api/events?token=SECRETVALUE123"; sleep 0.3
  if docker logs "$RUN-gw" 2>&1 | grep -q SECRETVALUE123; then echo "  FAIL token leaked into gateway log"; FAIL=1; else echo "  ok   token absent from gateway log"; fi
  # The 5 retained sources* routes (Python leadgen registry) never go to Go, even at 100%.
  for r in "GET /api/settings/sources" "GET /api/settings/sources/health" "PUT /api/settings/sources/x" \
           "POST /api/settings/sources/x/health/reset" "GET /api/settings/sources/registry"; do
    set -- $r
    expect "$r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "$r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # Unexposed by user decision: never proxied to either API (falls to the SPA location `/`)
  for p in /admin/users /scim/v2/x /scim/v2/Users; do
    expect "GET $p X-Stub (unset)" "$(hdr GET $p X-Stub)" ""
    expect "GET $p X-Upstream (unset)" "$(hdr GET $p X-Upstream)" ""
  done
}

# ===== plan 04: workbook CRUD split (retained routes stay on py at every percentage) =====
check_workbook_matrix() { # $1 = expected upstream for Go-eligible routes (py|go)
  local want="$1" r
  echo "== workbook CRUD split ($1) =="
  # Plan 06 retained routes and trailing-slash variants of the ported ones: always Python.
  for r in "GET /api/workbooks/7/sources/c1/preview" "GET /api/workbooks/meta/provider-accuracy" \
           "POST /api/v2/workbooks/7/generate-column" "POST /api/v2/workbooks/7/generate-column/" \
           "POST /api/workbooks/7/run/" "POST /api/workbooks/7/stop/" "POST /api/workbooks/7/rows/3/cells/c1/run/" \
           "POST /api/workbooks/7/refresh/" "GET /api/workbooks/7/ws/" "POST /api/v2/workbooks/7/ingest-token/" \
           "POST /api/v2/workbooks/7/rows/ingest/"; do
    set -- $r
    expect "retained $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "retained $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
    expect "retained $r X-Seen-Uri" "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  for r in "GET /api/workbooks" "GET /api/workbooks/" "POST /api/workbooks/" "GET /api/workbooks/7" "PATCH /api/workbooks/7" \
           "DELETE /api/workbooks/7" "GET /api/workbooks/7/runs" "GET /api/workbooks/7/rows/3/cells/c1/trace" \
           "PATCH /api/workbooks/7/rows" "POST /api/workbooks/7/rows/delete-query" "GET /api/workbooks/meta/providers" \
           "GET /api/workbooks/7/export/csv" "GET /api/v2/workbooks/7/views" "PUT /api/v2/workbooks/7/views/2" \
           "GET /api/templates" "GET /api/templates/gallery" "GET /api/functions" "GET /api/entities/company" \
           "GET /api/analytics/x" "GET /api/me/context" "GET /api/flags" "GET /api/system/stats" "GET /api/files/list"; do
    set -- $r
    expect "go-eligible $r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" "$want"
    expect "go-eligible $r X-Stub" "$(hdr "$1" "$2" X-Stub)" "$want"
    expect "go-eligible $r X-Seen-Uri" "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Lookalikes that must NOT be captured by the Go block.
  expect "GET /api/system-stats X-Stub" "$(hdr GET /api/system-stats X-Stub)" py
  expect "GET /api/workbooksx X-Stub" "$(hdr GET /api/workbooksx X-Stub)" py
  # Order: every retained regex location precedes the first Go regex location in the effective config.
  local T n_ret n_go; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  n_ret="$(printf '%s\n' "$T" | grep -n 'location ~ \^/api/workbooks/(meta/provider-accuracy' | head -1 | cut -d: -f1)"
  n_go="$(printf '%s\n' "$T" | grep -n 'location ~ \^/api/workbooks(/|\$)' | head -1 | cut -d: -f1)"
  if [ -n "$n_ret" ] && [ -n "$n_go" ] && [ "$n_ret" -lt "$n_go" ]; then echo "  ok   retained regex precedes Go regex"; else echo "  FAIL retained regex must precede Go regex ($n_ret vs $n_go)"; FAIL=1; fi
  # Timeouts/buffering of every plan 04 location equal `location /api/`.
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '~ ^/api/workbooks/(meta/provider-accuracy|[^/]+/sources/[^/]+/preview)$' \
           '~ ^/api/workbooks(/|$)' '~ ^/api/v2/workbooks/[^/]+/views(/|$)' '^~ /api/templates' '^~ /api/functions' \
           '^~ /api/entities' '^~ /api/analytics' '= /api/me/context' '= /api/flags' '= /api/system/stats' '^~ /api/files/'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
}
# ===== end plan 04 =====

# ===== plan 06: enrichment routes (17 ported follow the bucket, 3 retained stay on py; see check_workbook_matrix) =====
check_enrichment_matrix() { # $1 = expected upstream for ported routes (py|go)
  local want="$1" r
  echo "== enrichment split ($1) =="
  for r in "POST /api/workbooks/7/run" "POST /api/workbooks/7/rows/3/cells/c1/run" \
           "GET /api/workbooks/7/run/estimate" "POST /api/workbooks/7/stop" "GET /api/workbooks/meta/provider-stats" \
           "POST /api/workbooks/7/sources" "POST /api/workbooks/7/sources/c1/run" \
           "POST /api/workbooks/7/refresh" "PUT /api/workbooks/7/refresh-policy" "POST /api/workbooks/7/migrate" \
           "GET /api/workbooks/7/ws" "GET /api/workbooks/7/ws?token=T&workspace_id=ws-1" \
           "POST /api/v2/workbooks/7/ingest-token" "POST /api/v2/workbooks/7/rows/ingest"; do
    set -- $r
    expect "ported $r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" "$want"
    expect "ported $r X-Stub" "$(hdr "$1" "$2" X-Stub)" "$want"
    expect "ported $r X-Seen-Uri" "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Run and cell run are keyed per workspace: same workspace -> same bucket whatever the client; header absent -> client key.
  local a b
  a="$(curl -s -D- -o /dev/null -X POST -H 'X-Workspace-Id: ws-1' -H 'Authorization: Bearer one' "http://127.0.0.1:$PORT/api/workbooks/7/run" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
  b="$(curl -s -D- -o /dev/null -X POST -H 'X-Workspace-Id: ws-1' -H 'Authorization: Bearer two' "http://127.0.0.1:$PORT/api/workbooks/7/rows/3/cells/c1/run" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
  expect "run X-Upstream (workspace header)" "$a" "$want"
  expect "cell run X-Upstream (workspace header, other client)" "$b" "$want"
  # WebSocket upgrade headers reach the upstream.
  local ws; ws="$(curl -s -D- -o /dev/null -H 'Upgrade: websocket' -H 'Connection: Upgrade' "http://127.0.0.1:$PORT/api/workbooks/7/ws" | tr -d '\r')"
  expect "ws upstream HTTP version" "$(printf '%s\n' "$ws" | awk -F': ' 'tolower($1)=="x-seen-proto"{print $2}')" "HTTP/1.1"
  expect "ws Connection to upstream" "$(printf '%s\n' "$ws" | awk -F': ' 'tolower($1)=="x-seen-conn"{print $2}')" upgrade
  expect "ws Upgrade to upstream" "$(printf '%s\n' "$ws" | awk -F': ' 'tolower($1)=="x-seen-upgrade"{print $2}')" websocket
  # Retained routes: py at every percentage, no X-Upstream.
  for r in "GET /api/workbooks/7/sources/c1/preview" "GET /api/workbooks/meta/provider-accuracy" "POST /api/v2/workbooks/7/generate-column"; do
    set -- $r
    expect "retained $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "retained $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # Order and proxy settings of every plan 06 location.
  local T n_ret n_go; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  n_ret="$(printf '%s\n' "$T" | grep -n 'location ~ \^/api/workbooks/\[^/\]+/(run|rows' | head -1 | cut -d: -f1)"
  n_go="$(printf '%s\n' "$T" | grep -n 'location ~ \^/api/workbooks(/|\$)' | head -1 | cut -d: -f1)"
  if [ -n "$n_ret" ] && [ -n "$n_go" ] && [ "$n_ret" -lt "$n_go" ]; then echo "  ok   plan 06 regexes precede the plan 04 Go regex"; else echo "  FAIL plan 06 regexes must precede plan 04 Go regex ($n_ret vs $n_go)"; FAIL=1; fi
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '~ ^/api/workbooks/(meta/provider-accuracy|[^/]+/sources/[^/]+/preview)$' '~ ^/api/v2/workbooks/[^/]+/(generate-column/?|ingest-token/|rows/ingest/)$' \
           '~ ^/api/workbooks/[^/]+/(run|rows/[^/]+/cells/[^/]+/run)$' \
           '~ ^/api/workbooks/(meta/provider-stats|[^/]+/(run/estimate|stop|sources(/[^/]+/run)?|refresh(-policy)?|migrate))$' \
           '~ ^/api/v2/workbooks/[^/]+/(ingest-token|rows/ingest)$'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
  local w; w="$(blk '~ ^/api/workbooks/[^/]+/ws$')"
  case "$w" in *"proxy_read_timeout 1h;"*"proxy_send_timeout 1h;"*|*"proxy_read_timeout 1h;"*) echo "  ok   ws location has proxy_read_timeout 1h";; *) echo "  FAIL ws location lacks proxy_read_timeout 1h: $w"; FAIL=1;; esac
}
# ===== end plan 06 =====

# ===== plan 07: connectors, HubSpot, /api/data, AmbitionBox (all 19 routes ported, none retained) =====
check_connectors_matrix() { # $1 = expected upstream for Go-eligible routes (py|go)
  local want="$1" r
  echo "== connectors/crm split ($1) =="
  for r in "GET /api/connectors/catalog" "GET /api/connectors/compatibility" \
           "GET /api/crm/hubspot/status" "POST /api/crm/hubspot/connect" "POST /api/crm/hubspot/sync" "GET /api/crm/hubspot/contacts?limit=5" \
           "GET /api/data" "GET /api/data?limit=5&sort_by=email" "GET /api/data/export" "GET /api/data/stats/timeline" \
           "GET /api/data/stats/domains" "POST /api/data/mark-used" "PATCH /api/data/7" "DELETE /api/data/7" \
           "GET /api/ambitionbox/companies" "GET /api/ambitionbox/companies/12/jobs" "GET /api/ambitionbox/companies/12/detail" \
           "GET /api/ambitionbox/companies/12/similar" "GET /api/ambitionbox/collect" "POST /api/ambitionbox/imports"; do
    set -- $r
    expect "$r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" "$want"
    expect "$r X-Stub     " "$(hdr "$1" "$2" X-Stub)" "$want"
    expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Lookalikes, subpaths no Go route serves and trailing-slash variants stay on Python at every percentage.
  for r in "GET /api/crm/other" "GET /api/crm/hubspot" "GET /api/crm/hubspot/" "GET /api/crm/hubspot/status/x" "GET /api/crm/hubspotx/status" \
           "GET /api/data/" "GET /api/data-x" "GET /api/dataset" "GET /api/data/7/x" "GET /api/data/stats/other/x" \
           "GET /api/connectors" "GET /api/connectors/" "GET /api/connectors/x" "GET /api/connectors/catalog/x" "GET /api/connectorsx/catalog" \
           "GET /api/ambitionbox" "GET /api/ambitionbox/" "GET /api/ambitionbox/other" "GET /api/ambitionbox/companies/12" \
           "GET /api/ambitionbox/companies/12/other" "GET /api/ambitionbox/companies/12/jobs/x" "GET /api/ambitionboxx/companies"; do
    set -- $r
    expect "unmatched $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "unmatched $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # Proxy settings of each plan 07 location equal `location /api/`.
  local T; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '~ ^/api/connectors/(catalog|compatibility)$' '~ ^/api/crm/hubspot/(status|connect|sync|contacts)$' \
           '~ ^/api/data(/(export|mark-used|stats/(timeline|domains)|[^/]+))?$' \
           '~ ^/api/ambitionbox/(companies|collect|imports|companies/[^/]+/(jobs|detail|similar))$'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
}
# ===== end plan 07 =====

# ===== plan 08: automations and watches (all 18 routes ported, none retained) =====
check_automations_matrix() { # $1 = expected upstream for Go-eligible routes (py|go)
  local want="$1" r
  echo "== automations/watches split ($1) =="
  for r in "POST /api/automations/triggers" "GET /api/automations/triggers" "GET /api/automations/triggers/x" \
           "PATCH /api/automations/triggers/x" "DELETE /api/automations/triggers/x" "POST /api/automations/triggers/x/pause" \
           "POST /api/automations/triggers/x/resume" "POST /api/automations/triggers/x/preview" \
           "POST /api/automations/triggers/x/run" "GET /api/automations/triggers/x/runs?limit=5" "GET /api/automations/runs/7" \
           "POST /api/watches" "GET /api/watches" "GET /api/watches?limit=5" "GET /api/watches/x" "PATCH /api/watches/x" \
           "DELETE /api/watches/x" "POST /api/watches/x/poll" "GET /api/watches/x/signals"; do
    set -- $r
    expect "$r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" "$want"
    expect "$r X-Stub     " "$(hdr "$1" "$2" X-Stub)" "$want"
    expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Lookalikes stay on Python at every percentage (no retained route exists under either prefix).
  for r in "GET /api/automations" "GET /api/automationsx/triggers" "GET /api/watchesx" "GET /api/watches-x/1"; do
    set -- $r
    expect "unmatched $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "unmatched $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # Proxy settings of each plan 08 location equal `location /api/`.
  local T; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '~ ^/api/automations/' '~ ^/api/watches(/|$)'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
}
# ===== end plan 08 =====

echo "== nginx -t =="
start_gateway 0
if docker exec "$RUN-gw" nginx -t 2>&1; then echo "  ok   nginx -t"; else echo "  FAIL nginx -t"; FAIL=1; fi
check_matrix py 0%
check_workbook_matrix py
check_enrichment_matrix py
check_connectors_matrix py
check_automations_matrix py
start_gateway 100
docker exec "$RUN-gw" nginx -t >/dev/null 2>&1 || { echo "  FAIL nginx -t @100%"; FAIL=1; }
check_matrix go 100%
check_workbook_matrix go
check_enrichment_matrix go
check_connectors_matrix go
check_automations_matrix go

[ "$FAIL" = 0 ] && echo "ALL PASS" || { echo "FAILED"; exit 1; }
