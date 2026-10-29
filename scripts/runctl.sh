#!/usr/bin/env bash
# Process control for `make run` / `make dev` / `make kill` (state in .run/<name>.pid).
#   runctl.sh alive <name>   exit 0 if .run/<name>.pid names a live process that is OURS
#   runctl.sh kill           stop every verified process; stale or foreign pids are reported and never signalled
# "Ours" means the pid's /proc entry proves it: api/worker/scheduler -> exe is this repo's bin/opengtm-<name>;
# web -> cwd is this repo's apps/web and the command is bun/node/vite. The web pid is a session/group leader
# (started with setsid), so its whole group (bun -> vite -> esbuild) is signalled together.
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN="$ROOT/.run"
BIN="$ROOT/bin"

owned() { # name pid
  local name="$1" pid="$2" exe cwd cmd
  [[ "$pid" =~ ^[0-9]+$ ]] && [ "$pid" -gt 1 ] && [ -d "/proc/$pid" ] || return 1
  case "$name" in
    api|worker|scheduler)
      exe="$(readlink "/proc/$pid/exe" 2>/dev/null)" || return 1
      exe="${exe% (deleted)}"
      [ "$exe" = "$BIN/opengtm-$name" ] ;;
    web)
      cwd="$(readlink "/proc/$pid/cwd" 2>/dev/null)" || return 1
      [ "$cwd" = "$ROOT/apps/web" ] || return 1
      cmd="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null)"
      case "$cmd" in *bun*|*node*|*vite*) return 0;; *) return 1;; esac ;;
    *) return 1 ;;
  esac
}

pidof_file() { cat "$RUN/$1.pid" 2>/dev/null | tr -dc '0-9'; }

case "${1:-}" in
  alive)
    p="$(pidof_file "$2")"; [ -n "$p" ] && owned "$2" "$p" ;;
  kill)
    targets=()
    for f in "$RUN"/*.pid; do
      [ -f "$f" ] || continue
      n="$(basename "$f" .pid)"; p="$(pidof_file "$n")"
      if [ -n "$p" ] && owned "$n" "$p"; then targets+=("$n:$p")
      else echo "skip $n: pid '${p:-?}' is not a process started by this repo (stale pid file); not signalling" >&2; fi
    done
    sig() { # name pid signal
      if [ "$1" = web ] && [ "$(ps -o pgid= -p "$2" 2>/dev/null | tr -d ' ')" = "$2" ]; then kill "-$3" -- "-$2" 2>/dev/null
      else kill "-$3" "$2" 2>/dev/null; fi; }
    for t in "${targets[@]}"; do sig "${t%%:*}" "${t##*:}" TERM; done
    for _ in $(seq 20); do
      left=0; for t in "${targets[@]}"; do owned "${t%%:*}" "${t##*:}" && left=1; done
      [ "$left" = 0 ] && break; sleep 0.5
    done
    for t in "${targets[@]}"; do
      if owned "${t%%:*}" "${t##*:}"; then echo "KILL ${t##*:}"; sig "${t%%:*}" "${t##*:}" KILL; fi
    done
    rm -f "$RUN"/*.pid
    echo stopped ;;
  *) echo "usage: $0 alive <name> | kill" >&2; exit 2 ;;
esac
