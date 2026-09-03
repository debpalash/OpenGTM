#!/usr/bin/env bash
# Build the branch that will become the public repository, WITHOUT pushing it.
#
# The private repository's full history contains lead exports, private
# submodule pointers and internal strategy documents. Instead of rewriting
# that history, this script snapshots a ref into an orphan branch whose only
# ancestry is previous public snapshots, with the paths listed in
# public-exclude.txt removed. Nothing here touches the remote.
#
#   scripts/release/build-public-snapshot.sh                # snapshot HEAD → public/main
#   scripts/release/build-public-snapshot.sh v3.0.0         # snapshot a tag
#   PUBLIC_BRANCH=public/next scripts/release/build-public-snapshot.sh
#
# Afterwards, review the branch and push it yourself when you decide to publish:
#   git log --stat public/main
#   git push opengtm public/main:main
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"

src_ref="${1:-HEAD}"
branch="${PUBLIC_BRANCH:-public/main}"
src_commit="$(git rev-parse --verify "$src_ref^{commit}")"
short="$(git rev-parse --short "$src_commit")"

if [ "$src_ref" = "HEAD" ] && [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "Working tree has uncommitted changes; commit or stash them first (snapshots are taken from commits)." >&2
  exit 1
fi

tmp_index="$(mktemp)"
trap 'rm -f "$tmp_index"' EXIT

# Load the source tree into a scratch index and drop excluded paths.
GIT_INDEX_FILE="$tmp_index" git read-tree "$src_commit"
while IFS= read -r spec; do
  case "$spec" in ''|'#'*) continue ;; esac
  GIT_INDEX_FILE="$tmp_index" git rm -r -q --cached --ignore-unmatch -- "$spec" >/dev/null
done < "$here/public-exclude.txt"
tree="$(GIT_INDEX_FILE="$tmp_index" git write-tree)"

# Chain onto the previous public snapshot when one exists, so the public
# branch keeps a readable, private-history-free lineage.
parent_args=()
if git rev-parse --verify --quiet "refs/heads/$branch" >/dev/null; then
  if [ "$(git rev-parse "refs/heads/$branch^{tree}")" = "$tree" ]; then
    echo "$branch already matches $short; nothing to do."
    exit 0
  fi
  parent_args=(-p "$(git rev-parse "refs/heads/$branch")")
fi

msg="OpenGTM public snapshot of ${short}

Source-Commit: ${src_commit}
Built-By: scripts/release/build-public-snapshot.sh"
commit="$(printf '%s\n' "$msg" | git commit-tree "$tree" "${parent_args[@]}")"
git update-ref "refs/heads/$branch" "$commit"

echo "Created $branch at $(git rev-parse --short "$commit") from $short"
echo
"$here/preflight.sh" --snapshot "$branch" || { echo; echo "Snapshot built but preflight FAILED; do not publish it." >&2; exit 1; }
cat <<MSG

Next steps (manual, when you decide to publish):
  git log --stat -1 $branch
  git diff --stat $src_ref $branch      # confirms only excluded paths differ
  git push opengtm $branch:main         # <- the actual release; not run by this script
MSG
