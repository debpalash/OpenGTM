#!/usr/bin/env bash
# Public-release preflight: verify a git ref contains nothing that must not ship.
#
#   scripts/release/preflight.sh               # check HEAD
#   scripts/release/preflight.sh public/main   # check the snapshot branch
#   scripts/release/preflight.sh --snapshot public/main
#                                              # additionally require every path in
#                                              # public-exclude.txt to be absent
#
# Exit status is non-zero when any check fails. Read-only: never modifies the repo.
set -u

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
cd "$repo"

snapshot=0
ref="HEAD"
for arg in "$@"; do
  case "$arg" in
    --snapshot) snapshot=1 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) ref="$arg" ;;
  esac
done

if ! git rev-parse --verify --quiet "$ref^{commit}" >/dev/null; then
  echo "preflight: unknown ref '$ref'" >&2
  exit 2
fi

fail=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=1; }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; }

echo "Preflight for $(git rev-parse --short "$ref") ($ref)"
files="$(git ls-tree -r --name-only "$ref")"

# 1. Files that must never be committed.
echo "Forbidden files"
forbidden='(^|/)\.env(\..+)?$|\.(db|sqlite3?|dump|pem|key|p12|pfx|har)$|(^|/)id_(rsa|ed25519)|^backups/|^data/.*\.csv$|^data/(source_tests|pipeline_eval)/|mirror_state\.json$|br_import_checkpoint\.json$|(^|/)proxys\.txt$'
hits="$(printf '%s\n' "$files" | grep -E "$forbidden" | grep -vE '\.env\.example$' || true)"
if [ -n "$hits" ]; then bad "tracked files that must not ship:"; printf '%s\n' "$hits" | sed 's/^/      /'; else ok "no databases, dumps, env files, key material, or lead exports"; fi

# 2. Submodule pointers (public clones cannot fetch private submodules).
echo "Submodules"
if git ls-tree -r "$ref" | awk '$1=="160000"{print $4}' | grep -q .; then
  bad "gitlink entries present:"; git ls-tree -r "$ref" | awk '$1=="160000"{print "      "$4}'
elif printf '%s\n' "$files" | grep -qx '.gitmodules'; then
  bad ".gitmodules is tracked"
else
  ok "no submodule pointers"
fi

# 3. Required community files and lockfiles.
echo "Required files"
for f in LICENSE README.md SECURITY.md CONTRIBUTING.md CODE_OF_CONDUCT.md .env.example uv.lock bun.lock apps/docs/openapi/openapi.json; do
  if printf '%s\n' "$files" | grep -qx "$f"; then ok "$f"; else bad "missing $f"; fi
done

# 4. Secret patterns in tracked content (regex sweep; gitleaks below if installed).
#    Paths that never ship (public-exclude.txt) are skipped so the sweep reflects
#    what the public snapshot will contain.
echo "Secret sweep"
skip=(':!uv.lock' ':!bun.lock')
while IFS= read -r spec; do
  case "$spec" in ''|'#'*) continue ;; esac
  skip+=(":!$spec")
done < "$here/public-exclude.txt"
secret_re='sk-ant-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9]{32,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|xox[baprs]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{30,}|sk_live_[0-9a-zA-Z]{20,}|rk_live_[0-9a-zA-Z]{20,}|whsec_[0-9a-zA-Z]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY|hf_[A-Za-z0-9]{30,}|glpat-[A-Za-z0-9_-]{20,}|ycp_[A-Za-z0-9]{24,}|wbi_[A-Za-z0-9]{24,}'
hits="$(git grep -nIE "$secret_re" "$ref" -- . "${skip[@]}" 2>/dev/null | grep -vE 'example|placeholder|REDACTED|\.\.\.' || true)"
if [ -n "$hits" ]; then bad "possible credentials:"; printf '%s\n' "$hits" | sed "s#^$ref:##; s#^#      #" | head -20; else ok "no credential-shaped strings"; fi
# Absolute home paths leak usernames and never work for anyone else.
hits="$(git grep -nIE '(^|[[:space:]"'"'"'=(:,])(/home/[a-z][a-z0-9_-]*|/Users/[A-Za-z][A-Za-z0-9_-]*)/' "$ref" -- . "${skip[@]}" ':!*.md' 2>/dev/null || true)"
if [ -n "$hits" ]; then bad "hard-coded home directories:"; printf '%s\n' "$hits" | sed "s#^$ref:##; s#^#      #" | head -10; else ok "no hard-coded home directories in code"; fi
if command -v gitleaks >/dev/null 2>&1; then
  if gitleaks git --no-banner --redact --log-opts="$ref -1" "$repo" >/dev/null 2>&1; then ok "gitleaks (last commit)"; else bad "gitleaks reported findings (run: gitleaks git --log-opts='$ref -1')"; fi
else
  warn "gitleaks not installed; only the regex sweep ran (https://github.com/gitleaks/gitleaks)"
fi

# 5. Snapshot-only: excluded paths must be gone.
if [ "$snapshot" -eq 1 ]; then
  echo "Excluded paths"
  while IFS= read -r spec; do
    case "$spec" in ''|'#'*) continue ;; esac
    if printf '%s\n' "$files" | grep -qE "^${spec//./\\.}"; then bad "still present: $spec"; else ok "absent: $spec"; fi
  done < "$here/public-exclude.txt"
fi

echo
if [ "$fail" -eq 0 ]; then echo "PASS"; else echo "FAIL — fix the items marked ✗ before publishing"; fi
exit "$fail"
