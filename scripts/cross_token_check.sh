#!/usr/bin/env bash
# Cross-token check: a token minted by Python must work on Go and vice versa (same SECRET_KEY).
# Needs LIVE services (not run in CI). Talks to the services directly, not through nginx.
#   PY_URL=http://127.0.0.1:8000 GO_URL=http://127.0.0.1:8001 \
#   CHECK_EMAIL=... CHECK_PASSWORD=... scripts/cross_token_check.sh
# Credentials come from the environment only; tokens go through temp header files (never argv/logs).
set -euo pipefail
: "${PY_URL:?}" "${GO_URL:?}" "${CHECK_EMAIL:?}" "${CHECK_PASSWORD:?}"
TMP="$(mktemp -d)"; chmod 700 "$TMP"; trap 'rm -rf "$TMP"' EXIT
FAIL=0

mint() { # $1 base url, $2 out prefix -> writes $2.access / $2.refresh as curl header/body files
  python3 -c 'import os,urllib.parse as u;print(u.urlencode({"username":os.environ["CHECK_EMAIL"],"password":os.environ["CHECK_PASSWORD"]}),end="")' \
    | curl -sf -X POST "$1/auth/token" -H 'Content-Type: application/x-www-form-urlencoded' -d @- >"$2.json"
  python3 - "$2" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]+".json"))
open(sys.argv[1]+".access","w").write("Authorization: Bearer %s\n"%d["access_token"])
open(sys.argv[1]+".refresh","w").write(json.dumps({"refresh_token":d["refresh_token"]}))
PY
  rm -f "$2.json"
}
status() { curl -s -o /dev/null -w '%{http_code}' -H @"$2" "$1/auth/me"; }
expect() { if [ "$2" = "$3" ]; then echo "ok   $1 = $2"; else echo "FAIL $1: got $2 want $3"; FAIL=1; fi; }

mint "$PY_URL" "$TMP/py"; mint "$GO_URL" "$TMP/go"
expect "py-minted access on go /auth/me" "$(status "$GO_URL" "$TMP/py.access")" 200
expect "go-minted access on py /auth/me" "$(status "$PY_URL" "$TMP/go.access")" 200
# Refresh tokens cross over too, and a refresh token must NOT work as an access token anywhere.
for pair in "py:$GO_URL" "go:$PY_URL"; do
  src="${pair%%:*}"; dst="${pair#*:}"
  code="$(curl -s -o "$TMP/r.out" -w '%{http_code}' -X POST "$dst/auth/refresh" \
    -H 'Content-Type: application/json' -d @"$TMP/$src.refresh")"
  expect "$src-minted refresh on $dst /auth/refresh" "$code" 200
done
python3 - "$TMP" <<'PY'
import json,sys
t=sys.argv[1]
for n in ("py","go"):
    rt=json.load(open(f"{t}/{n}.refresh"))["refresh_token"]
    open(f"{t}/{n}.asaccess","w").write("Authorization: Bearer %s\n"%rt)
PY
for n in py go; do for u in "$PY_URL" "$GO_URL"; do
  expect "$n refresh token as access on $u" "$(status "$u" "$TMP/$n.asaccess")" 401
done; done
[ "$FAIL" = 0 ] && echo "CROSS-TOKEN OK" || { echo "CROSS-TOKEN FAILED"; exit 1; }
