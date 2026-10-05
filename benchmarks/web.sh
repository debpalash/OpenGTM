#!/usr/bin/env bash
# Web baseline for apps/web. See benchmarks/README.md ("Web baseline").
#
#   ./benchmarks/web.sh [--quick] [--label NAME] [--reps N] [--skip-build] [--bundle-only]
#
# Builds apps/web (bun run build), measures bundle sizes per route from the Vite
# manifest, then drives the built SPA with headless Chromium (Playwright, from
# uv.lock) against a local static server and fixture API. It does not start or
# touch the Go server, FastAPI, any database, or ./data.
# Requires: bun, uv, and the Playwright browsers (`uv run playwright install chromium`).
set -euo pipefail
cd "$(dirname "$0")/.."

for tool in bun uv; do
  command -v "$tool" >/dev/null || { echo "error: '$tool' not found on PATH" >&2; exit 2; }
done

exec uv run --frozen python benchmarks/web.py "$@"
