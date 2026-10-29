#!/usr/bin/env bash
# Build the image and run it twice with the compose hardening (read-only root,
# tmpfs, cap_drop ALL, no-new-privileges, the Chromium seccomp profile) and no
# published ports: once as uid 10001 (default), once in split mode (root with
# only SETUID/SETGID, Chromium as uid 10002). Each run renders a local page,
# checks that internal targets are refused and that Chromium is sandboxed,
# runs as the expected uid and cannot read the service token.
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE="${IMAGE:-opengtm-browser:local}"
NAME="opengtm-browser-smoke-$$"
[[ "${SKIP_BUILD:-}" == 1 ]] || docker build -q -t "$IMAGE" . >/dev/null
TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
HARDEN=(--read-only --tmpfs /tmp --tmpfs /home/browser --cap-drop ALL --security-opt no-new-privileges
  --security-opt "seccomp=$PWD/seccomp-chromium.json" --security-opt apparmor=unconfined --shm-size 1g)
# without a token the service must refuse to start
if docker run --rm "${HARDEN[@]}" "$IMAGE" >/dev/null 2>&1; then echo "started without a token"; exit 1; fi

run_mode() { # $1 = label, $2 = chromium uid, $3 = exec user, rest = extra docker run args
  local label=$1 uid=$2 exec_user=$3; shift 3
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker run -d --name "$NAME" "${HARDEN[@]}" "$@" -e BROWSER_SERVICE_TOKEN="$TOKEN" -e APP_ENV=test \
    -e BROWSER_EGRESS_ALLOW_CIDRS=127.0.0.1/32 -e BROWSER_EGRESS_PORTS=80,443,18080 "$IMAGE" >/dev/null
  for _ in $(seq 1 40); do
    docker exec "$NAME" python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8090/healthz', timeout=2)" \
      >/dev/null 2>&1 && break
    sleep 1
  done
  echo "== $label"
  docker exec -i -u "$exec_user" -e SMOKE_TOKEN="$TOKEN" -e SMOKE_CHROMIUM_UID="$uid" "$NAME" python - < scripts/smoke_in_container.py
}

run_mode "uid 10001 (default)" 10001 10001
run_mode "split mode" 10002 10001 --user 0 --cap-add SETUID --cap-add SETGID
