#!/usr/bin/env bash
# Build the image, start it with no published ports, render a local page inside
# the container and check that internal targets are refused. Removes the container.
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE="${IMAGE:-opengtm-browser:local}"
NAME="opengtm-browser-smoke-$$"
[[ "${SKIP_BUILD:-}" == 1 ]] || docker build -q -t "$IMAGE" . >/dev/null
TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
# without a token the service must refuse to start
if docker run --rm "$IMAGE" >/dev/null 2>&1; then echo "started without a token"; exit 1; fi
docker run -d --name "$NAME" -e BROWSER_SERVICE_TOKEN="$TOKEN" -e APP_ENV=test \
  -e BROWSER_EGRESS_ALLOW_CIDRS=127.0.0.1/32 "$IMAGE" >/dev/null
for _ in $(seq 1 30); do
  docker exec "$NAME" python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8090/healthz', timeout=2)" \
    >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$NAME" id -u | grep -qx 10001 || { echo "container is not running as uid 10001"; exit 1; }
docker exec -i "$NAME" python - < scripts/smoke_in_container.py
