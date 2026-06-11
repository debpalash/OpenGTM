#!/usr/bin/env bash
# Build/refresh Yupcha's SearXNG integration branch: official SearXNG with the
# useful community-fork commits overlaid on top, so we keep getting community
# goodies without losing upstream. Idempotent — re-run any time to pull the
# latest from everyone.
#
# Strategy (a "union fork"):
#   upstream = searxng/searxng           (the base of truth)
#   + privau/searxng    (deploy/theming/env-var goodies)
#   + tiekoetter/searxng (instance-operator patches)
#   + return42/searxng   (core-maintainer prototypes)
#   → merged into branch `yupcha-integration`
#
# Conflicts on a given fork are skipped (that fork's merge is aborted) and
# reported, so one messy fork never blocks the others. Review + resolve those
# manually if you want them.
#
# ── One-time setup of the submodule (do this once, with YOUR fork) ──
#   1. Fork searxng/searxng on GitHub  -> github.com/yupcha-internal/searxng
#   2. git submodule add https://github.com/yupcha-internal/searxng apps/searxng
#   3. cd apps/searxng && ../../infra/searxng/sync-fork.sh .
#   4. git push origin yupcha-integration   # publish to your fork
#   5. Build the image from apps/searxng and point infra/searxng at it.
#
# Until then, run it against a throwaway clone to preview the merge:
#   ./infra/searxng/sync-fork.sh /tmp/searxng-union
set -euo pipefail

DIR="${1:-apps/searxng}"
BRANCH="${SEARXNG_INTEGRATION_BRANCH:-yupcha-integration}"

UPSTREAM="https://github.com/searxng/searxng.git"
# Community forks to overlay. Order = merge order (later wins on auto-merge).
declare -A FORKS=(
  [privau]="https://github.com/privau/searxng.git"
  [tiekoetter]="https://github.com/tiekoetter/searxng.git"
  [return42]="https://github.com/return42/searxng.git"
)

log() { printf '\033[1;36m▸ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m! %s\033[0m\n' "$*"; }

if [ ! -d "$DIR/.git" ]; then
  log "cloning upstream SearXNG into $DIR"
  git clone --filter=blob:none "$UPSTREAM" "$DIR"
fi
cd "$DIR"

log "fetching upstream"
git remote get-url upstream >/dev/null 2>&1 || git remote add upstream "$UPSTREAM"
git fetch --quiet upstream

# Determine upstream default branch (master or main).
UPBR="$(git remote show upstream | sed -n 's/.*HEAD branch: //p')"
log "upstream default branch: $UPBR"

log "(re)creating $BRANCH from upstream/$UPBR"
git checkout -B "$BRANCH" "upstream/$UPBR"

MERGED=(); SKIPPED=()
for name in "${!FORKS[@]}"; do
  url="${FORKS[$name]}"
  git remote get-url "$name" >/dev/null 2>&1 || git remote add "$name" "$url"
  log "fetching $name"
  if ! git fetch --quiet "$name" 2>/dev/null; then
    warn "$name: fetch failed, skipping"; SKIPPED+=("$name(fetch)"); continue
  fi
  fbr="$(git remote show "$name" | sed -n 's/.*HEAD branch: //p')"
  log "merging $name/$fbr"
  if git merge --no-edit -m "merge $name/$fbr into $BRANCH" "$name/$fbr"; then
    MERGED+=("$name")
  else
    warn "$name: merge conflicts — aborting this fork's merge (resolve manually if wanted)"
    git merge --abort
    SKIPPED+=("$name(conflict)")
  fi
done

echo
log "DONE — branch '$BRANCH' is upstream/$UPBR + [${MERGED[*]:-none}]"
[ ${#SKIPPED[@]} -gt 0 ] && warn "skipped: ${SKIPPED[*]}"
echo "Review:  git log --oneline --graph -20"
echo "Publish: git push origin $BRANCH    (to your own fork)"
