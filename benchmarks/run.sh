#!/usr/bin/env bash
# One-command M0 baseline benchmark. See benchmarks/README.md.
#
#   OPENGTM_BENCH_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:55432/postgres \
#     ./benchmarks/run.sh [--quick] [--label NAME] [--only queue|http] ...
#
# The URL must be a superuser URL of a DISPOSABLE PostgreSQL server. Only
# throwaway databases and roles are created in it, and dropped afterwards.
# Pass --help for every knob. Requires: go, uv (Python env from uv.lock).
set -euo pipefail
cd "$(dirname "$0")/.."

for tool in go uv; do
  command -v "$tool" >/dev/null || { echo "error: '$tool' not found on PATH" >&2; exit 2; }
done

exec uv run --frozen python benchmarks/run.py "$@"
