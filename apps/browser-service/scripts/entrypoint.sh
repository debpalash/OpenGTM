#!/bin/sh
# Browser service entrypoint (review M1).
#
# Started as uid 10001 (the image default): runs the service; Chromium is a
# child of the same uid, with its sandbox on and no token in its environment
# (the service drops the token from its environment and turns off dumping
# before Playwright starts).
#
# Started as root with only SETUID/SETGID (compose: user "0", cap_drop ALL,
# cap_add [SETUID, SETGID]): split mode. A Playwright server (and a patchright
# one with BROWSER_MAPS_STEALTH) runs as uid 10002 on loopback with a random
# path and an environment holding nothing but PATH/HOME; the service runs as
# uid 10001 and connects to it. Both drop every capability.
set -eu

if [ "$(id -u)" != "0" ]; then
  exec python -m browser_service
fi

secret() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
drop_to() { uid=$1; shift; setpriv --reuid="$uid" --regid="$uid" --clear-groups --inh-caps=-all --bounding-set=-all --no-new-privs "$@"; }
CLEAN_PATH=/app/.venv/bin:/usr/local/bin:/usr/bin:/bin

pw_port=${BROWSER_PLAYWRIGHT_PORT:-8095}
pw_path=$(secret)
drop_to 10002 env -i PATH="$CLEAN_PATH" HOME=/tmp PLAYWRIGHT_BROWSERS_PATH=/ms-playwright playwright run-server --host 127.0.0.1 --port "$pw_port" --path "/$pw_path" --unsafe >/dev/null &
export BROWSER_PLAYWRIGHT_WS="ws://127.0.0.1:$pw_port/$pw_path"

case "${BROWSER_MAPS_STEALTH:-}" in
  1|true|yes|on)
    pr_port=$((pw_port + 1))
    pr_path=$(secret)
    drop_to 10002 env -i PATH="$CLEAN_PATH" HOME=/tmp PLAYWRIGHT_BROWSERS_PATH=/ms-playwright patchright run-server --host 127.0.0.1 --port "$pr_port" --path "/$pr_path" --unsafe >/dev/null &
    export BROWSER_PATCHRIGHT_WS="ws://127.0.0.1:$pr_port/$pr_path"
    ;;
esac

exec setpriv --reuid=10001 --regid=10001 --clear-groups --inh-caps=-all --bounding-set=-all --no-new-privs \
  env HOME=/home/browser python -m browser_service
