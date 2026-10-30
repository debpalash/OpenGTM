#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
set -a
source .env
set +a
export DATABASE_URL="$APP_DATABASE_URL"
pids=()
cleanup(){ trap - EXIT INT TERM; kill "${pids[@]}" 2>/dev/null || true; wait "${pids[@]}" 2>/dev/null || true; }
trap cleanup EXIT
trap 'exit 0' INT TERM
bin/opengtm-api > .run/api.log 2>&1 &
pids+=("$!")
bin/worker > .run/worker.log 2>&1 &
pids+=("$!")
bin/scheduler > .run/scheduler.log 2>&1 &
pids+=("$!")
printf '%s\n' "${pids[@]}" > .run/native.pids
wait
