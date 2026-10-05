#!/usr/bin/env bash
# Print the git ref to treat as "the previous release" for the upgrade tests.
#
# Preference order:
#   1. PREV_REF, if set.
#   2. The newest version tag before HEAD whose tree can run under the install
#      compose file (it needs apps/api/scripts/provision_runtime_role.py and
#      seed_demo.py). Releases cut before `opengtm init` existed do not qualify.
#   3. The fork point from origin/main, or HEAD's first parent when HEAD is
#      already on main. This is the fallback until a qualifying release exists;
#      the e2e logs say which ref was used.
set -euo pipefail

if [[ -n "${PREV_REF:-}" ]]; then
  echo "$PREV_REF"
  exit 0
fi

qualifies() {
  git cat-file -e "$1:apps/api/scripts/provision_runtime_role.py" 2>/dev/null &&
    git cat-file -e "$1:apps/api/scripts/seed_demo.py" 2>/dev/null
}

head_sha="$(git rev-parse HEAD)"
while read -r tag; do
  [[ -n "$tag" ]] || continue
  [[ "$(git rev-parse "$tag^{commit}")" == "$head_sha" ]] && continue
  if qualifies "$tag"; then
    echo "$tag"
    exit 0
  fi
done < <(git tag --list 'v[0-9]*' --sort=-v:refname --merged HEAD)

base="$(git merge-base origin/main HEAD 2>/dev/null || true)"
if [[ -z "$base" || "$base" == "$head_sha" ]]; then
  base="$(git rev-parse HEAD^ 2>/dev/null || echo "$head_sha")"
fi
echo "$base"
