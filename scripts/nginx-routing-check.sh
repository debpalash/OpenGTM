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
  # First-admin setup: always Go at any percentage (Python has no such route), URI forwarded unchanged.
  for r in "GET /auth/setup/status" "POST /auth/setup"; do
    set -- $r
    expect "$r X-Stub" "$(hdr "$1" "$2" X-Stub)" go
    expect "$r X-Seen-Uri" "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
    expect "$r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # Workspace SSO login (S-M5): always Go at any percentage, URI (with query) forwarded unchanged.
  for r in "GET /auth/sso/x" "GET /auth/sso/x/login" "GET /auth/sso/x/callback?code=c&state=s"; do
    set -- $r
    expect "$r X-Stub" "$(hdr "$1" "$2" X-Stub)" go
    expect "$r X-Seen-Uri" "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
    expect "$r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # Prefix boundary: /auth/ssox is not an SSO route and stays on Python.
  expect "GET /auth/ssox X-Stub" "$(hdr GET /auth/ssox X-Stub)" py
  # Retained lead/signal routes (testdata/contract/retained_leads_routes.txt): always Python at any percentage.
  # The plan 14 routes that list used to include (collect, jobs*, system-stats, lead utilities) are ported now and
  # follow the bucket (check_leadgen_matrix); only enrich, unified search and scraper stay here.
  for r in "POST /api/lead/7/enrich" "GET /api/v2/search/unified" "POST /api/v2/scraper/scrape" "GET /api/signals/sources"; do
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
  expect "GET /api/system-statsx X-Stub" "$(hdr GET /api/system-statsx X-Stub)" py  # lookalike of /api/system/stats (/api/system-stats itself is plan 14)
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

# ===== plan 12: copilot chat and person intel (all 12 routes ported, none retained) =====
check_copilot_matrix() { # $1 = expected upstream for Go-eligible routes (py|go)
  local want="$1" r
  echo "== copilot/person split ($1) =="
  for r in "POST /api/copilotkit" "GET /api/copilotkit/info" "POST /api/copilotkit/info" "GET /api/copilotkit/conversations" \
           "GET /api/copilotkit/conversations/abc123" "DELETE /api/copilotkit/conversations/abc123" "GET /api/copilotkit/memories" \
           "POST /api/person/enrich" "GET /api/person/history" "GET /api/person/history?skip=1&limit=2" "GET /api/person/7" \
           "DELETE /api/person/7"; do
    set -- $r
    expect "$r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" "$want"
    expect "$r X-Stub     " "$(hdr "$1" "$2" X-Stub)" "$want"
    expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # /ws/person/enrich is Go only: never proxied to Python (404 in the Python bucket, no upstream reached).
  for r in "GET /ws/person/enrich" "GET /ws/person/enrich?token=SECRET"; do
    set -- $r
    if [ "$want" = go ]; then
      expect "$r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" go
      expect "$r X-Stub     " "$(hdr "$1" "$2" X-Stub)" go
      expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
    else
      expect "$r status (Python bucket: 404)" "$(code "$1" "$2")" 404
      expect "$r X-Stub (no upstream reached)" "$(hdr "$1" "$2" X-Stub)" ""
    fi
  done
  # Lookalikes, unknown subpaths and trailing-slash variants stay on Python at every percentage.
  for r in "GET /api/copilotkit/" "POST /api/copilotkit/" "GET /api/copilotkit/other" "GET /api/copilotkit/conversations/" \
           "GET /api/copilotkit/conversations/a/b" "GET /api/copilotkitx" "GET /api/copilotkit/memories/x" \
           "GET /api/person/" "GET /api/person/7/x" "GET /api/personx" "GET /api/person"; do
    set -- $r
    expect "unmatched $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "unmatched $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # The copilotkit routes are keyed per workspace: same X-Workspace-Id -> same bucket whatever the client.
  local a b
  a="$(curl -s -D- -o /dev/null -X POST -H 'X-Workspace-Id: ws-1' -H 'Authorization: Bearer one' "http://127.0.0.1:$PORT/api/copilotkit" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
  b="$(curl -s -D- -o /dev/null -H 'X-Workspace-Id: ws-1' -H 'Authorization: Bearer two' "http://127.0.0.1:$PORT/api/copilotkit/conversations" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
  expect "chat X-Upstream (workspace header)" "$a" "$want"
  expect "conversations X-Upstream (workspace header, other client)" "$b" "$want"
  # WebSocket upgrade headers reach the upstream (Go bucket only); the token stays out of the gateway log.
  local ws; ws="$(curl -s -D- -o /dev/null -H 'Upgrade: websocket' -H 'Connection: Upgrade' "http://127.0.0.1:$PORT/ws/person/enrich?token=T" | tr -d '\r')"
  if [ "$want" = go ]; then
    expect "ws upstream HTTP version" "$(printf '%s\n' "$ws" | awk -F': ' 'tolower($1)=="x-seen-proto"{print $2}')" "HTTP/1.1"
    expect "ws Connection to upstream" "$(printf '%s\n' "$ws" | awk -F': ' 'tolower($1)=="x-seen-conn"{print $2}')" upgrade
    expect "ws Upgrade to upstream" "$(printf '%s\n' "$ws" | awk -F': ' 'tolower($1)=="x-seen-upgrade"{print $2}')" websocket
  fi
  curl -s -o /dev/null -H 'Upgrade: websocket' "http://127.0.0.1:$PORT/ws/person/enrich?token=PERSONSECRET456"; sleep 0.3
  if docker logs "$RUN-gw" 2>&1 | grep -q PERSONSECRET456; then echo "  FAIL person ws token leaked into gateway log"; FAIL=1; else echo "  ok   person ws token absent from gateway log"; fi
  # Location settings (not observable from a stub): compare blocks in `nginx -T`.
  local T; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '= /api/copilotkit/info' '= /api/copilotkit/conversations' '~ ^/api/copilotkit/conversations/[^/]+$' '= /api/copilotkit/memories' \
           '~ ^/api/person/[^/]+$' '= /ws/person/enrich'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
  # Chat location: SSE settings; request buffering and X-Accel-Buffering handling are left at the defaults.
  local C; C="$(printf '%s\n' "$T" | awk 'index($0,"location = /api/copilotkit {"){f=1;next} f&&/^[[:space:]]*}/{exit} f')"
  for d in "proxy_buffering off;" "proxy_read_timeout 1h;" "proxy_http_version 1.1;" "add_header X-Upstream \$go_ws_bucket always;"; do
    case "$C" in *"$d"*) echo "  ok   POST /api/copilotkit has '$d'";; *) echo "  FAIL POST /api/copilotkit missing '$d'"; FAIL=1;; esac
  done
  C="$(printf '%s\n' "$C" | sed 's/^[[:space:]]*#.*//')" # directives only, not comments
  case "$C" in *proxy_request_buffering*|*proxy_ignore_headers*) echo "  FAIL chat location must not set proxy_request_buffering/proxy_ignore_headers"; FAIL=1;; *) echo "  ok   chat location leaves request buffering and X-Accel-Buffering at defaults";; esac
  # Ordering: no earlier regex location may capture these paths (first matching regex wins).
  local W; W="$(printf '%s\n' "$T" | awk 'index($0,"location = /ws/person/enrich {"){f=1;next} f&&/^[[:space:]]*}/{exit} f')"
  case "$W" in *"proxy_read_timeout 300s;"*) echo "  ok   person ws location has proxy_read_timeout 300s";; *) echo "  FAIL person ws location lacks proxy_read_timeout 300s"; FAIL=1;; esac
}
# Plan 12 at an intermediate percentage: the copilotkit routes are keyed per workspace (stickiness), the person WS is Go only.
check_copilot_split50() {
  echo "== copilot/person split (50%) =="
  local w c a b seen="" ws_go=0 ws_py=0 stick=1
  for w in ws-1 ws-2 ws-3 ws-4 ws-5 ws-6 ws-7 ws-8 ws-9 ws-10 ws-11 ws-12; do
    # same workspace, different clients and routes -> one bucket
    a="$(curl -s -D- -o /dev/null -X POST -H "X-Workspace-Id: $w" -H 'Authorization: Bearer one' "http://127.0.0.1:$PORT/api/copilotkit" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
    b="$(curl -s -D- -o /dev/null -H "X-Workspace-Id: $w" -H 'Authorization: Bearer two' "http://127.0.0.1:$PORT/api/copilotkit/conversations" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
    c="$(curl -s -D- -o /dev/null -H "X-Workspace-Id: $w" -H 'Authorization: Bearer three' "http://127.0.0.1:$PORT/api/copilotkit/memories" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
    if [ -z "$a" ] || [ "$a" != "$b" ] || [ "$a" != "$c" ]; then stick=0; echo "  FAIL $w not sticky: chat=$a conversations=$b memories=$c"; fi
    seen="$seen $a"
  done
  [ "$stick" = 1 ] && echo "  ok   chat, conversations and memories agree per workspace"
  case "$seen" in *go*) ws_go=1;; esac
  case "$seen" in *py*) ws_py=1;; esac
  if [ "$ws_go" = 1 ] && [ "$ws_py" = 1 ]; then echo "  ok   both upstreams appear across workspaces"; else echo "  FAIL workspaces all landed on one upstream:$seen"; FAIL=1; fi
  [ "$stick" = 1 ] || FAIL=1
  # Person WS: 200 (stub) exactly when the client bucket is go, else 404; never the Python stub.
  local i up st
  for i in 1 2 3 4 5 6 7 8; do
    up="$(curl -s -D- -o /dev/null -H "Authorization: Bearer c$i" "http://127.0.0.1:$PORT/api/person/history" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
    st="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer c$i" "http://127.0.0.1:$PORT/ws/person/enrich")"
    if [ "$up" = go ]; then expect "client c$i ws (go bucket)" "$st" 200; else expect "client c$i ws (py bucket)" "$st" 404; fi
  done
}
# ===== end plan 12 =====

# ===== plan 11: governance and playbooks (26 ported, 2 capabilities routes retained) =====
check_governance_matrix() { # $1 = expected upstream for Go-eligible routes (py|go)
  local want="$1" r
  echo "== governance/playbooks split ($1) =="
  # The two retained /capabilities routes are always Python, at every percentage.
  for r in "GET /api/governance/capabilities" "GET /api/research-playbooks/capabilities" "PATCH /api/research-playbooks/capabilities"; do
    set -- $r
    expect "retained $r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" py
    expect "retained $r X-Stub     " "$(hdr "$1" "$2" X-Stub)" py
  done
  for r in "GET /api/governance/audit-events" "GET /api/governance/audit-events?limit=5&actor_user_id=2" "GET /api/governance/audit-events/export.csv" \
           "GET /api/governance/retention" "PUT /api/governance/retention" "POST /api/governance/retention/preview" \
           "POST /api/governance/retention/enforce" "GET /api/governance/retention/runs?limit=5" \
           "GET /api/governance/rbac" "PUT /api/governance/rbac/2/activation.write" "POST /api/governance/members" \
           "PATCH /api/governance/members/2" "DELETE /api/governance/members/2" "GET /api/governance/sso" "PUT /api/governance/sso" \
           "GET /api/governance/scim-token" "POST /api/governance/scim-token" "DELETE /api/governance/scim-token" \
           "GET /api/research-playbooks" "POST /api/research-playbooks" "PATCH /api/research-playbooks/pb-1" "DELETE /api/research-playbooks/pb-1" \
           "POST /api/research-playbooks/pb-1/runs" "GET /api/research-playbooks/pb-1/runs?limit=5" \
           "GET /api/research-playbooks/runs/r-1/results" "POST /api/research-playbooks/runs/r-1/retry" "POST /api/research-playbooks/runs/r-1/cancel"; do
    set -- $r
    expect "$r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" "$want"
    expect "$r X-Stub     " "$(hdr "$1" "$2" X-Stub)" "$want"
    expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Lookalikes, unknown subpaths and trailing-slash variants stay on Python at every percentage.
  for r in "GET /api/governance" "GET /api/governance/" "GET /api/governance/other" "GET /api/governancex/sso" "GET /api/governance/sso/" \
           "GET /api/governance/retention/other" "GET /api/governance/retention/enforce/x" "GET /api/governance/rbac/2" \
           "GET /api/governance/members/2/x" "GET /api/governance/audit-events/other" "GET /api/governance/capabilities/x" \
           "GET /api/research-playbooks/" "GET /api/research-playbooks/pb-1/other" "GET /api/research-playbooks/pb-1/runs/" \
           "GET /api/research-playbooks/runs/r-1" "GET /api/research-playbooks/runs/r-1/other" "GET /api/research-playbooksx"; do
    set -- $r
    expect "unmatched $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "unmatched $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # Enforce and run start are keyed per workspace: same X-Workspace-Id -> same bucket whatever the client.
  local a b c
  a="$(curl -s -D- -o /dev/null -X POST -H 'X-Workspace-Id: ws-1' -H 'Authorization: Bearer one' "http://127.0.0.1:$PORT/api/governance/retention/enforce" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
  b="$(curl -s -D- -o /dev/null -X POST -H 'X-Workspace-Id: ws-1' -H 'Authorization: Bearer two' "http://127.0.0.1:$PORT/api/research-playbooks/pb-1/runs" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
  c="$(curl -s -D- -o /dev/null -X POST -H 'X-Workspace-Id: ws-1' -H 'Authorization: Bearer three' "http://127.0.0.1:$PORT/api/research-playbooks/pb-2/runs" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}')"
  expect "enforce X-Upstream (workspace header)" "$a" "$want"
  expect "run start X-Upstream (workspace header, other client)" "$b" "$want"
  expect "run start X-Upstream (workspace header, other playbook)" "$c" "$want"
  # Location settings (not observable from a stub): compare blocks in `nginx -T`.
  local T; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '= /api/governance/audit-events' '= /api/governance/retention/enforce' '~ ^/api/research-playbooks/[^/]+/runs$' '= /api/governance/sso'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
  local E; E="$(blk '= /api/governance/audit-events/export.csv')"
  for d in "proxy_buffering off;" "proxy_read_timeout 300s;"; do
    case "$E" in *"$d"*) echo "  ok   export.csv has '$d'";; *) echo "  FAIL export.csv missing '$d'"; FAIL=1;; esac
  done
}
# ===== end plan 11 =====

# ===== plan 09: outreach (22 /api/outreach routes ported, POST /api/campaigns/generate retained) =====
check_outreach_matrix() { # $1 = expected upstream for Go-eligible routes (py|go|any: 50% bucket, only consistency is asserted)
  local want="$1" r u s
  echo "== outreach split ($1) =="
  # The retained LLM route is always Python, at every percentage.
  set -- POST /api/campaigns/generate
  expect "retained $1 $2 X-Upstream" "$(hdr "$1" "$2" X-Upstream)" py
  expect "retained $1 $2 X-Stub     " "$(hdr "$1" "$2" X-Stub)" py
  for r in "GET /api/outreach/drafts" "GET /api/outreach/drafts?limit=5&offset=1" "GET /api/outreach/drafts/d-1" \
           "GET /api/outreach/sequences" "POST /api/outreach/sequences" "GET /api/outreach/sequences/s-1" \
           "PUT /api/outreach/sequences/s-1" "DELETE /api/outreach/sequences/s-1" "POST /api/outreach/sequences/s-1/start" \
           "POST /api/outreach/sequences/s-1/pause" "POST /api/outreach/sequences/s-1/enroll" "POST /api/outreach/sequences/s-1/execute" \
           "GET /api/outreach/sequences/s-1/stats" "GET /api/outreach/sequences/s-1/sends" \
           "GET /api/outreach/smtp/status" "PUT /api/outreach/smtp/config" "POST /api/outreach/smtp/test" \
           "GET /api/outreach/suppressions" "POST /api/outreach/suppressions" "DELETE /api/outreach/suppressions/a%40example.com" \
           "GET /api/outreach/unsubscribe?token=abc.def" "POST /api/outreach/unsubscribe?token=abc.def" "POST /api/outreach/webhooks/bounce"; do
    set -- $r
    u="$(hdr "$1" "$2" X-Upstream)"; s="$(hdr "$1" "$2" X-Stub)"
    if [ "$want" = any ]; then
      case "$u" in py|go) expect "$r X-Stub == X-Upstream" "$s" "$u";; *) echo "  FAIL $r X-Upstream '$u' not py|go"; FAIL=1;; esac
    else
      expect "$r X-Upstream" "$u" "$want"
      expect "$r X-Stub     " "$s" "$want"
    fi
    expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Lookalikes, unknown subpaths and trailing-slash variants stay on Python at every percentage.
  for r in "GET /api/outreach" "GET /api/outreach/" "GET /api/outreach/other" "GET /api/outreachx/drafts" "GET /api/outreach/drafts/" \
           "GET /api/outreach/drafts/d-1/x" "GET /api/outreach/sequences/" "GET /api/outreach/sequences/s-1/other" \
           "GET /api/outreach/sequences/s-1/stats/" "GET /api/outreach/smtp" "GET /api/outreach/smtp/other" "GET /api/outreach/suppressions/" \
           "GET /api/outreach/unsubscribe/" "GET /api/outreach/webhooks" "GET /api/outreach/webhooks/bounce/" "GET /api/outreach/webhooks/other" \
           "GET /api/campaigns" "GET /api/campaigns/generate/x" "POST /api/campaignsx/generate"; do
    set -- $r
    expect "unmatched $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "unmatched $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # Public routes are sticky per request source: ten requests from one address (no Authorization header, as in a mail
  # client's one-click POST or a provider webhook) reach one stack, and unsubscribe and webhook agree.
  local i seen="" w
  for i in 1 2 3 4 5 6 7 8 9 10; do
    seen="$seen $(hdr POST "/api/outreach/unsubscribe?token=t$i" X-Upstream) $(hdr POST /api/outreach/webhooks/bounce X-Upstream)"
  done
  w="$(printf '%s\n' $seen | sort -u | tr '\n' ' ')"; w="${w% }"
  case "$w" in
    py|go) if [ "$want" = any ]; then echo "  ok   public routes sticky upstream = $w"; else expect "public routes sticky upstream" "$w" "$want"; fi;;
    *) echo "  FAIL public routes not sticky/consistent: '$w'"; FAIL=1;;
  esac
  # Landing/one-click links keep resolving: the GET landing and the POST land on the same stack.
  expect "unsubscribe GET and POST share a stack" "$(hdr GET '/api/outreach/unsubscribe?token=z' X-Upstream)" "$(hdr POST '/api/outreach/unsubscribe?token=z' X-Upstream)"
  # Location settings (not observable from a stub): compare blocks in `nginx -T`.
  local T; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '= /api/outreach/drafts' '~ ^/api/outreach/sequences/[^/]+/(start|pause|enroll|execute|stats|sends)$' '= /api/outreach/smtp/test' \
           '~ ^/api/outreach/suppressions/[^/]+$' '= /api/outreach/unsubscribe' '= /api/outreach/webhooks/bounce' '= /api/campaigns/generate'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
  # The public locations forward the client address so the limiter sees the client, not nginx.
  for l in '= /api/outreach/unsubscribe' '= /api/outreach/webhooks/bounce'; do
    local B; B="$(printf '%s\n' "$T" | awk -v pat="$l" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f')"
    for d in 'proxy_set_header X-Real-IP $remote_addr;' 'proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;' 'add_header X-Upstream $go_bucket always;'; do
      case "$B" in *"$d"*) echo "  ok   '$l' has '$d'";; *) echo "  FAIL '$l' missing '$d'"; FAIL=1;; esac
    done
  done
  # The retained route is pinned to api_backend regardless of the bucket.
  local R; R="$(printf '%s\n' "$T" | awk 'index($0,"location = /api/campaigns/generate {"){f=1;next} f&&/^[[:space:]]*}/{exit} f')"
  case "$R" in *"proxy_pass http://api_backend;"*) echo "  ok   campaigns/generate pinned to api_backend";; *) echo "  FAIL campaigns/generate not pinned to api_backend"; FAIL=1;; esac
}
# ===== end plan 09 =====

# ===== plan 10: audiences and destinations (23 routes ported, 2 catalog routes retained) =====
check_audiences_matrix() { # $1 = expected upstream for ported routes (py|go|any: 50% bucket, only consistency is asserted)
  local want="$1" r u s
  echo "== audiences/destinations split ($1) =="
  # The retained certification catalogs are always Python, at every percentage.
  for r in "GET /api/audiences/capabilities" "GET /api/audience-destinations/types"; do
    set -- $r
    expect "retained $r X-Upstream" "$(hdr "$1" "$2" X-Upstream)" py
    expect "retained $r X-Stub     " "$(hdr "$1" "$2" X-Stub)" py
  done
  for r in "GET /api/audiences" "GET /api/audiences?limit=5" "POST /api/audiences" "PATCH /api/audiences/a-1" "DELETE /api/audiences/a-1" \
           "POST /api/audiences/a-1/refresh" "GET /api/audiences/a-1/members?limit=5" "GET /api/audiences/a-1/accounts" "GET /api/audiences/a-1/events" \
           "GET /api/audience-destinations" "POST /api/audience-destinations" "PATCH /api/audience-destinations/d-1" "DELETE /api/audience-destinations/d-1" \
           "POST /api/audience-destinations/d-1/sync" "GET /api/audience-destinations/d-1/runs" "GET /api/audience-destinations/d-1/health" \
           "GET /api/audience-destinations/runs/r-1/deliveries" "POST /api/audience-destinations/runs/r-1/retry" "POST /api/audience-destinations/runs/r-1/cancel" \
           "POST /api/audience-destinations/d-1/inbound-token" "GET /api/audience-destinations/d-1/inbound-token" "DELETE /api/audience-destinations/d-1/inbound-token" \
           "GET /api/audience-destinations/d-1/inbound-receipts" "POST /api/audience-destinations/inbound/d-1" "POST /api/audience-destinations/inbound/sync"; do
    set -- $r
    u="$(hdr "$1" "$2" X-Upstream)"; s="$(hdr "$1" "$2" X-Stub)"
    if [ "$want" = any ]; then
      case "$u" in py|go) expect "$r X-Stub == X-Upstream" "$s" "$u";; *) echo "  FAIL $r X-Upstream '$u' not py|go"; FAIL=1;; esac
    else
      expect "$r X-Upstream" "$u" "$want"
      expect "$r X-Stub     " "$s" "$want"
    fi
    expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Lookalikes, unknown subpaths and trailing-slash variants stay on Python at every percentage (and are not newly exposed).
  for r in "GET /api/audiences/" "GET /api/audiences/a-1/other" "GET /api/audiences/a-1/members/" "GET /api/audiencesx" "GET /api/audiences/a-1/refresh/extra" \
           "GET /api/audience-destinations/" "GET /api/audience-destinations/d-1/other" "GET /api/audience-destinations/runs/r-1/other" \
           "POST /api/audience-destinations/d-1/inbound/x" "POST /api/audience-destinations/inbound/d-1/" "GET /api/audience-destination"; do
    set -- $r
    expect "unmatched $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "unmatched $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # The public callback is sticky per request source and agrees with the other client-bucket routes of that source.
  local i seen="" w
  for i in 1 2 3 4 5 6 7 8 9 10; do seen="$seen $(hdr POST /api/audience-destinations/inbound/d-$i X-Upstream)"; done
  w="$(printf '%s\n' $seen | sort -u | tr '\n' ' ')"; w="${w% }"
  case "$w" in
    py|go) if [ "$want" = any ]; then echo "  ok   public callback sticky upstream = $w"; else expect "public callback sticky upstream" "$w" "$want"; fi;;
    *) echo "  FAIL public callback not sticky/consistent: '$w'"; FAIL=1;;
  esac
  # Location settings (not observable from a stub): compare blocks in `nginx -T`.
  local T; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&match($0,/^ *}/)&&RLENGTH<=5{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '= /api/audiences' '~ ^/api/audiences/[^/]+/refresh$' '~ ^/api/audience-destinations/[^/]+/(sync|runs|health|inbound-token|inbound-receipts)$' \
           '~ ^/api/audience-destinations/inbound/[^/]+$' '= /api/audiences/capabilities'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
  # The public callback forwards the client address so the limiter sees the client, not nginx.
  local B; B="$(printf '%s\n' "$T" | awk 'index($0,"location ~ ^/api/audience-destinations/inbound/[^/]+$ {"){f=1;next} f&&/^[[:space:]]*}/{exit} f')"
  for d in 'proxy_set_header X-Real-IP $remote_addr;' 'proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;' 'add_header X-Upstream $go_bucket always;'; do
    case "$B" in *"$d"*) echo "  ok   inbound callback has '$d'";; *) echo "  FAIL inbound callback missing '$d'"; FAIL=1;; esac
  done
  # The retained routes are pinned to api_backend regardless of the bucket.
  for l in '= /api/audiences/capabilities' '= /api/audience-destinations/types'; do
    local R; R="$(printf '%s\n' "$T" | awk -v pat="$l" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f')"
    case "$R" in *"proxy_pass http://api_backend;"*) echo "  ok   '$l' pinned to api_backend";; *) echo "  FAIL '$l' not pinned to api_backend"; FAIL=1;; esac
  done
}

# Plan 10 at an intermediate percentage: create, patch and refresh are keyed per workspace (one workspace, one stack, for
# every route and every client), the reads follow the client bucket.
check_audiences_split50() {
  echo "== audiences/destinations split (50%) =="
  local w a b c seen="" stick=1 go=0 py=0
  up() { curl -s -D- -o /dev/null -X "$1" -H "X-Workspace-Id: $2" -H "Authorization: Bearer $3" "http://127.0.0.1:$PORT$4" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}'; }
  for w in ws-1 ws-2 ws-3 ws-4 ws-5 ws-6 ws-7 ws-8 ws-9 ws-10 ws-11 ws-12; do
    a="$(up POST "$w" one /api/audiences)"
    b="$(up POST "$w" two /api/audiences/a-1/refresh)"
    c="$(up PATCH "$w" three /api/audiences/a-2)"
    if [ -z "$a" ] || [ "$a" != "$b" ] || [ "$a" != "$c" ]; then stick=0; echo "  FAIL $w not sticky: create=$a refresh=$b patch=$c"; fi
    seen="$seen $a"
  done
  [ "$stick" = 1 ] && echo "  ok   create, refresh and patch agree per workspace"
  case "$seen" in *go*) go=1;; esac
  case "$seen" in *py*) py=1;; esac
  if [ "$go" = 1 ] && [ "$py" = 1 ]; then echo "  ok   both upstreams appear across workspaces"; else echo "  FAIL workspaces all landed on one upstream:$seen"; FAIL=1; fi
  [ "$stick" = 1 ] || FAIL=1
  # A GET (and the sticky routes' other methods) of the same path follows the client bucket, not the workspace.
  local i x y
  for i in 1 2 3 4 5 6; do
    x="$(up GET ws-1 "c$i" /api/audiences)"; y="$(up GET ws-2 "c$i" /api/audiences)"
    expect "GET /api/audiences client c$i ignores the workspace" "$x" "$y"
  done
}
# ===== end plan 10 =====

# ===== plan 14: leadgen collection (19 ported, 3 retained) =====
check_leadgen_matrix() { # $1 = expected upstream for Go-eligible routes (py|go|any)
  local want="$1" r u
  echo "== leadgen collection split ($1) =="
  # Retained on FastAPI: always Python and no X-Upstream header, at every percentage.
  for r in "POST /api/lead/7/enrich" "GET /api/v2/search/unified" "POST /api/v2/scraper/scrape"; do
    set -- $r
    expect "retained $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "retained $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  for r in "POST /api/collect" "GET /api/jobs" "GET /api/jobs?status=done&limit=5" "GET /api/jobs/j-1" "DELETE /api/jobs/j-1" \
           "GET /api/jobs/j-1/stages" "GET /api/jobs/j-1/leads" "GET /api/jobs/j-1/events?limit=5" "POST /api/jobs/j-1/cancel" \
           "POST /api/jobs/j-1/retry" "GET /api/system-stats" "POST /api/leads/score" "POST /api/leads/verify-email" \
           "POST /api/leads/tech-stack" "POST /api/leads/domain-intel" "GET /api/settings/sources" "GET /api/settings/sources/health" \
           "GET /api/settings/sources/registry" "PUT /api/settings/sources/google_maps" "POST /api/settings/sources/google_maps/health/reset"; do
    set -- $r
    u="$(hdr "$1" "$2" X-Upstream)"
    if [ "$want" = any ]; then
      case "$u" in go|py) echo "  ok   $r X-Upstream = $u";; *) echo "  FAIL $r X-Upstream: got '$u'"; FAIL=1;; esac
      expect "$r X-Stub matches X-Upstream" "$(hdr "$1" "$2" X-Stub)" "$u"
    else
      expect "$r X-Upstream" "$u" "$want"
      expect "$r X-Stub     " "$(hdr "$1" "$2" X-Stub)" "$want"
    fi
    expect "$r X-Seen-Uri " "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
  done
  # Lookalikes, unknown subpaths and trailing-slash variants stay on Python at every percentage.
  for r in "GET /api/jobs/" "GET /api/jobsx" "GET /api/jobs/j-1/other" "GET /api/jobs/j-1/stages/" "GET /api/jobs/j-1/leads/x" \
           "GET /api/collect/" "GET /api/collectx" "GET /api/system-stats/" "GET /api/leads/score/" "GET /api/leads/other" \
           "GET /api/leads/score/x" "GET /api/settings/sources/" "GET /api/settings/sources/x/y" "GET /api/settings/sources/x/health" \
           "GET /api/settings/sources/x/health/reset/" "GET /api/settings/sourcesx" "GET /api/lead/7/enrich/" "GET /api/v2/search/unified/"; do
    set -- $r
    expect "unmatched $r X-Stub" "$(hdr "$1" "$2" X-Stub)" py
    expect "unmatched $r X-Upstream (unset)" "$(hdr "$1" "$2" X-Upstream)" ""
  done
  # collect and every jobs route are keyed per workspace: same X-Workspace-Id -> same bucket whatever the client or the route.
  local a b c d
  up() { curl -s -D- -o /dev/null -X "$1" -H "X-Workspace-Id: $2" -H "Authorization: Bearer $3" "http://127.0.0.1:$PORT$4" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}'; }
  a="$(up POST ws-1 one /api/collect)"; b="$(up GET ws-1 two /api/jobs)"; c="$(up POST ws-1 three /api/jobs/j-9/retry)"; d="$(up DELETE ws-1 four /api/jobs/j-8)"
  [ "$want" = any ] || { expect "collect X-Upstream (workspace header)" "$a" "$want"; expect "jobs list X-Upstream (workspace header, other client)" "$b" "$want"; }
  if [ -n "$a" ] && [ "$a" = "$b" ] && [ "$a" = "$c" ] && [ "$a" = "$d" ]; then echo "  ok   collect, list, retry and delete agree for one workspace ($a)"; else echo "  FAIL ws-1 not sticky: collect=$a list=$b retry=$c delete=$d"; FAIL=1; fi
  # Location settings (not observable from a stub): the proxy settings equal the `/api/` location.
  local T; T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
  blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f' | grep -E 'proxy_(read_timeout|send_timeout|buffering|http_version)' | sed 's/^[[:space:]]*//' | sort; }
  local l
  for l in '= /api/collect' '~ ^/api/jobs(/[^/]+(/(stages|leads|events|cancel|retry))?)?$' '= /api/system-stats' \
           '~ ^/api/leads/(score|verify-email|tech-stack|domain-intel)$' '~ ^/api/settings/sources(/(health|registry|[^/]+|[^/]+/health/reset))?$'; do
    expect "timeouts/buffering of '$l' == '/api/'" "$(blk "$l")" "$(blk "/api/")"
  done
  # The retained routes are pinned to api_backend regardless of the bucket.
  for l in '~ ^/api/lead/[^/]+/enrich$' '= /api/v2/search/unified' '= /api/v2/scraper/scrape'; do
    local R; R="$(printf '%s\n' "$T" | awk -v pat="$l" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f')"
    case "$R" in *"proxy_pass http://api_backend;"*) echo "  ok   '$l' pinned to api_backend";; *) echo "  FAIL '$l' not pinned to api_backend"; FAIL=1;; esac
  done
}

# Plan 14 at an intermediate percentage: collect and the jobs routes are keyed per workspace (one workspace, one stack, for every
# route and every client); the utilities follow the client bucket.
check_leadgen_split50() {
  echo "== leadgen collection split (50%) =="
  local w a b c d seen="" stick=1 go=0 py=0 i x y
  up() { curl -s -D- -o /dev/null -X "$1" -H "X-Workspace-Id: $2" -H "Authorization: Bearer $3" "http://127.0.0.1:$PORT$4" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-upstream"{print $2}'; }
  for w in ws-1 ws-2 ws-3 ws-4 ws-5 ws-6 ws-7 ws-8 ws-9 ws-10 ws-11 ws-12; do
    a="$(up POST "$w" one /api/collect)"; b="$(up GET "$w" two /api/jobs)"; c="$(up POST "$w" three /api/jobs/j-1/retry)"; d="$(up POST "$w" four /api/jobs/j-1/cancel)"
    if [ -z "$a" ] || [ "$a" != "$b" ] || [ "$a" != "$c" ] || [ "$a" != "$d" ]; then stick=0; echo "  FAIL $w not sticky: collect=$a list=$b retry=$c cancel=$d"; fi
    seen="$seen $a"
  done
  [ "$stick" = 1 ] && echo "  ok   collect, list, retry and cancel agree per workspace"
  case "$seen" in *go*) go=1;; esac
  case "$seen" in *py*) py=1;; esac
  if [ "$go" = 1 ] && [ "$py" = 1 ]; then echo "  ok   both upstreams appear across workspaces"; else echo "  FAIL workspaces all landed on one upstream:$seen"; FAIL=1; fi
  [ "$stick" = 1 ] || FAIL=1
  # System stats, the utilities and the source settings follow the client, not the workspace.
  for i in 1 2 3 4 5 6; do
    x="$(up GET ws-1 "c$i" /api/system-stats)"; y="$(up GET ws-2 "c$i" /api/system-stats)"
    expect "GET /api/system-stats client c$i ignores the workspace" "$x" "$y"
    x="$(up POST ws-1 "c$i" /api/leads/score)"; y="$(up GET ws-2 "c$i" /api/settings/sources)"
    expect "score and source settings agree for client c$i" "$x" "$y"
  done
}
# ===== end plan 14 =====

echo "== nginx -t =="
start_gateway 0
if docker exec "$RUN-gw" nginx -t 2>&1; then echo "  ok   nginx -t"; else echo "  FAIL nginx -t"; FAIL=1; fi
check_matrix py 0%
check_workbook_matrix py
check_enrichment_matrix py
check_connectors_matrix py
check_automations_matrix py
check_copilot_matrix py
check_governance_matrix py
check_outreach_matrix py
check_audiences_matrix py
check_leadgen_matrix py
start_gateway 50
docker exec "$RUN-gw" nginx -t >/dev/null 2>&1 || { echo "  FAIL nginx -t @50%"; FAIL=1; }
check_outreach_matrix any
check_audiences_matrix any
check_audiences_split50
check_copilot_split50
check_leadgen_matrix any
check_leadgen_split50
start_gateway 100
docker exec "$RUN-gw" nginx -t >/dev/null 2>&1 || { echo "  FAIL nginx -t @100%"; FAIL=1; }
check_matrix go 100%
check_workbook_matrix go
check_enrichment_matrix go
check_connectors_matrix go
check_automations_matrix go
check_copilot_matrix go
check_governance_matrix go
check_outreach_matrix go
check_audiences_matrix go
check_leadgen_matrix go

[ "$FAIL" = 0 ] && echo "ALL PASS" || { echo "FAILED"; exit 1; }
