#!/usr/bin/env bash
# Routing check for the Go-only gateway (plan 15): runs a throwaway nginx (pinned image) with the REAL
# infra/nginx/default.conf and a stub upstream aliased `api-go` on a private docker network, plus a decoy stub
# aliased `api` (the retired Python API) that must never be reached. Asserts `nginx -t`, that every route reaches
# api-go with its URI unchanged, the proxy headers, the WebSocket/SSE settings, the token-safe logs and the 404s.
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

expect() { # desc got want
  if [ "$2" = "$3" ]; then echo "  ok   $1 = $2"; else echo "  FAIL $1: got '$2' want '$3'"; FAIL=1; fi; }

echo "== config files =="
for f in infra/nginx/go-routes.conf infra/nginx/go-split.conf; do
  if [ -e "$f" ]; then echo "  FAIL $f still exists"; FAIL=1; else echo "  ok   $f removed"; fi
done
if grep -nE 'api_backend|server api:|go_bucket|split_clients' infra/nginx/default.conf; then
  echo "  FAIL default.conf still references the Python upstream or the canary"; FAIL=1
else echo "  ok   default.conf has no Python upstream or canary"; fi

stub_conf() { # $1 = name
  cat > "$TMP/stub-$1.conf" <<CONF
server { listen 8000; location / {
  add_header X-Stub $1 always; add_header X-Seen-Uri \$request_uri always;
  add_header X-Seen-Proto \$server_protocol always; add_header X-Seen-Conn \$http_connection always;
  add_header X-Seen-Upgrade \$http_upgrade always; add_header X-Seen-Host \$http_host always;
  add_header X-Seen-Real-Ip \$http_x_real_ip always; add_header X-Seen-Fwd-Proto \$http_x_forwarded_proto always;
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

docker run -d --name "$RUN-gw" --network "$RUN" -p 127.0.0.1::80 \
  -v "$PWD/infra/nginx/default.conf:/etc/nginx/conf.d/default.conf:ro" "$IMG" >/dev/null
sleep 0.5; PORT="$(docker port "$RUN-gw" 80/tcp 2>/dev/null | head -1 | sed "s/.*://")"
[ -n "$PORT" ] || { docker logs "$RUN-gw" 2>&1 | tail -8; exit 1; }
for _ in $(seq 40); do curl -s -o /dev/null "http://127.0.0.1:$PORT/health" && break; sleep 0.25; done

hdr() { # method path header [extra curl args...]
  local m="$1" p="$2" k="$3"; shift 3
  curl -s -D- -o /dev/null -X "$m" "$@" "http://127.0.0.1:$PORT$p" | tr -d '\r' | awk -F': ' -v k="$k" 'tolower($1)==tolower(k){print $2}'; }
code() { curl -s -o /dev/null -w '%{http_code}' -X "$1" "http://127.0.0.1:$PORT$2"; }

echo "== nginx -t =="
if docker exec "$RUN-gw" nginx -t 2>&1; then echo "  ok   nginx -t"; else echo "  FAIL nginx -t"; FAIL=1; fi
T="$(docker exec "$RUN-gw" nginx -T 2>/dev/null)"
blk() { printf '%s\n' "$T" | awk -v pat="$1" 'index($0,"location " pat " {"){f=1;next} f&&/^[[:space:]]*}/{exit} f'; }
has() { # location directive...
  local l="$1" b; shift; b="$(blk "$l")"
  for d in "$@"; do case "$b" in *"$d"*) echo "  ok   '$l' has '$d'";; *) echo "  FAIL '$l' missing '$d'"; FAIL=1;; esac; done; }

echo "== every route reaches api-go, URI unchanged =="
# One or more routes per former plan area (formerly split between Python and Go), including the routes that
# used to stay on Python (retained, trailing-slash variants, unknown siblings).
for r in "GET /" "GET /setup" "GET /assets/index-abc.js" "GET /workbooks/7" "GET /health" \
         "POST /auth/token" "POST /auth/refresh" "GET /auth/me" "GET /auth/setup/status" "POST /auth/setup" \
         "GET /auth/sso/x" "GET /auth/sso/x/login" "GET /auth/ssox" \
         "GET /api/workspaces" "GET /api/workspaces/1/members" "GET /api/mcp/tokens" "DELETE /api/mcp/tokens/3" \
         "GET /api/settings/providers" "PUT /api/settings/providers/groq" "GET /api/settings/system" \
         "GET /api/settings/sources/registry" "GET /api/billing/balance" "POST /api/billing/webhook" \
         "GET /api/leads?limit=5" "GET /api/leads/" "POST /api/lead/7/enrich" "GET /api/lead/7/timeline" \
         "GET /api/export/csv" "POST /api/leads/bulk-enrich" "GET /api/signals" "GET /api/signals/sources" \
         "POST /api/events/ticket" "POST /api/events/ticketx" "GET /api/v2/search/unified" "POST /api/v2/scraper/scrape" \
         "GET /api/workbooks" "GET /api/workbooks/" "PATCH /api/workbooks/7" "POST /api/workbooks/7/run" \
         "POST /api/workbooks/7/run/" "POST /api/workbooks/7/rows/3/cells/c1/run" "GET /api/workbooks/7/run/estimate" \
         "GET /api/workbooks/meta/provider-stats" "GET /api/workbooks/7/sources/c1/preview" \
         "POST /api/v2/workbooks/7/rows/ingest" "GET /api/connectors/catalog" "GET /api/crm/hubspot/status" \
         "GET /api/data?limit=5&sort_by=email" "GET /api/ambitionbox/companies/12" "GET /api/automations/triggers" \
         "POST /api/automations/triggers/x/run" "GET /api/watches/x/signals" "POST /api/copilotkit" \
         "GET /api/copilotkit/conversations" "GET /api/person/history?skip=1&limit=2" \
         "GET /api/governance/capabilities" "GET /api/governance/audit-events/export.csv" \
         "POST /api/governance/retention/enforce" "GET /api/research-playbooks/capabilities" \
         "POST /api/research-playbooks/pb-1/runs" "GET /api/outreach/sequences/s-1/stats" \
         "GET /api/outreach/unsubscribe?token=abc.def" "POST /api/outreach/webhooks/bounce" \
         "POST /api/campaigns/generate" "GET /api/audiences/capabilities" "POST /api/audiences/a-1/refresh" \
         "GET /api/audience-destinations/types" "POST /api/audience-destinations/d-1/sync" \
         "POST /api/collect" "GET /api/jobs/j-1/stages" "GET /api/system-stats" "GET /api/trending" "GET /api/sources"; do
  set -- $r
  expect "$r X-Stub    " "$(hdr "$1" "$2" X-Stub)" go
  expect "$r X-Seen-Uri" "$(hdr "$1" "$2" X-Seen-Uri)" "$2"
done

echo "== proxy headers =="
expect "Host forwarded" "$(hdr GET /api/leads X-Seen-Host -H 'Host: crm.example.com')" crm.example.com
expect "X-Real-IP set" "$(hdr GET /api/leads X-Seen-Real-Ip)" "$(hdr GET /auth/me X-Seen-Real-Ip)"
[ -n "$(hdr GET /api/leads X-Seen-Real-Ip)" ] && echo "  ok   X-Real-IP non-empty" || { echo "  FAIL X-Real-IP empty"; FAIL=1; }
for p in /api/leads /auth/me / /api/events /api/copilotkit /ws/person/enrich /auth/sso/x /api/workbooks/7/ws; do
  expect "$p X-Forwarded-Proto" "$(hdr GET "$p" X-Seen-Fwd-Proto)" http
  expect "$p upstream HTTP version" "$(hdr GET "$p" X-Seen-Proto)" "HTTP/1.1"
done
echo "== security headers (exactly one copy) =="
for p in / /api/leads /api/events /auth/me; do
  expect "$p X-Frame-Options" "$(hdr GET "$p" X-Frame-Options)" SAMEORIGIN
  expect "$p X-Content-Type-Options" "$(hdr GET "$p" X-Content-Type-Options)" nosniff
done

echo "== WebSocket upgrade =="
for p in /api/leads /api/workbooks/7/ws /api/workbooks/7/ws/ "/ws/person/enrich?token=SECRET"; do
  expect "$p Connection (upgrade req)" "$(hdr GET "$p" X-Seen-Conn -H 'Upgrade: websocket' -H 'Connection: Upgrade')" upgrade
  expect "$p Upgrade (upgrade req)" "$(hdr GET "$p" X-Seen-Upgrade -H 'Upgrade: websocket' -H 'Connection: Upgrade')" websocket
done
for p in /api/leads /api/events /api/copilotkit; do
  expect "$p Connection (plain req)" "$(hdr GET "$p" X-Seen-Conn)" ""
done

echo "== stream settings =="
has "= /api/events" "proxy_buffering off;" "proxy_read_timeout 1h;" "add_header X-Accel-Buffering no always;" \
    "access_log /dev/stdout sse_noquery;" "error_log /dev/stderr crit;" "limit_conn sse_per_ip 20;" "limit_conn_status 429;"
expect "GET /api/events X-Accel-Buffering" "$(hdr GET '/api/events?token=SECRET' X-Accel-Buffering)" no
has "~ ^/api/workbooks/[^/]+/ws/?$" "proxy_read_timeout 1h;" "access_log /dev/stdout sse_noquery;" "error_log /dev/stderr crit;"
has "= /api/copilotkit" "proxy_buffering off;" "proxy_read_timeout 1h;"
has "= /ws/person/enrich" "access_log /dev/stdout sse_noquery;" "error_log /dev/stderr crit;"
has "^~ /auth/sso/" "access_log /dev/stdout sse_noquery;"
has "/api/" "proxy_buffering off;" "proxy_read_timeout 300s;" "proxy_send_timeout 300s;"

echo "== credentials never logged =="
for u in "/api/events?token=SECRETVALUE111" "/api/workbooks/7/ws?token=SECRETVALUE222" \
         "/ws/person/enrich?token=SECRETVALUE333" "/auth/sso/x/callback?code=SECRETVALUE444&state=s"; do
  curl -s -o /dev/null "http://127.0.0.1:$PORT$u"
done
sleep 0.3
if docker logs "$RUN-gw" 2>&1 | grep -q 'SECRETVALUE'; then echo "  FAIL credential leaked into gateway log"; FAIL=1
else echo "  ok   credentials absent from gateway log"; fi

echo "== not exposed (404 at the gateway, never proxied) =="
for p in /internal/v1/credentials /admin/users /scim/v2/Users /ws/other /ws; do
  expect "GET $p status" "$(code GET "$p")" 404
  expect "GET $p X-Stub (unset)" "$(hdr GET "$p" X-Stub)" ""
done

echo "== the retired Python decoy was never reached =="
if docker logs "$RUN-py" 2>&1 | grep -q '"GET\|"POST'; then echo "  FAIL a request reached the decoy 'api' upstream"; FAIL=1
else echo "  ok   decoy 'api' upstream untouched"; fi

[ "$FAIL" = 0 ] && echo "ALL PASS" || { echo "FAILED"; exit 1; }
