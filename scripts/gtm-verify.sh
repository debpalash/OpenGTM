#!/usr/bin/env bash
# Re-verify every number GOAL.md §2 asserts. Prints a diff against what was
# true on 2026-07-16.
#
# Why this exists: GOAL.md is a snapshot and it rots. The one rule that caught
# every real finding in this project was "check the artifact, not the claim
# about the artifact" — so the claims ship with a way to check themselves.
#
# A check that cannot run prints GAP, not a zero. A data gap is not a zero.
#
# Usage: scripts/gtm-verify.sh

set -uo pipefail

BASELINE_DATE="2026-07-16"
pass=0; fail=0; gap=0

c_red=$'\033[31m'; c_grn=$'\033[32m'; c_yel=$'\033[33m'; c_dim=$'\033[2m'; c_off=$'\033[0m'

# check <label> <expected> <actual>
check() {
  local label="$1" expected="$2" actual="$3"
  if [ -z "$actual" ] || [ "$actual" = "GAP" ]; then
    printf '  %sGAP %s  %s\n' "$c_yel" "$c_off" "$label"
    printf '      %scould not measure — NOT a zero%s\n' "$c_dim" "$c_off"
    gap=$((gap+1))
  elif [ "$actual" = "$expected" ]; then
    printf '  %sOK  %s  %-46s %s\n' "$c_grn" "$c_off" "$label" "$actual"
    pass=$((pass+1))
  else
    printf '  %sDIFF%s  %-46s %s%s%s (was %s)\n' \
      "$c_red" "$c_off" "$label" "$c_red" "$actual" "$c_off" "$expected"
    fail=$((fail+1))
  fi
}

# Numbers that move on their own. Report, never assert.
drift() {
  local label="$1" baseline="$2" actual="$3"
  if [ -z "$actual" ]; then
    printf '  %sGAP %s  %s\n' "$c_yel" "$c_off" "$label"; gap=$((gap+1)); return
  fi
  printf '  %s~~  %s  %-46s %s %s(was %s on %s)%s\n' \
    "$c_dim" "$c_off" "$label" "$actual" "$c_dim" "$baseline" "$BASELINE_DATE" "$c_off"
}

echo
echo "GTM verify — GOAL.md §2 vs reality   ${c_dim}(baseline ${BASELINE_DATE})${c_off}"
echo

# ── OmniVoice-Studio ────────────────────────────────────────────────────────
echo "OmniVoice-Studio"
drift "stars"     "8509"   "$(gh api repos/debpalash/OmniVoice-Studio --jq .stargazers_count 2>/dev/null)"
drift "forks"     "1362"   "$(gh api repos/debpalash/OmniVoice-Studio --jq .forks_count 2>/dev/null)"
drift "downloads (563 assets — NOT people)" "136707" \
  "$(gh api --paginate repos/debpalash/OmniVoice-Studio/releases \
       --jq '.[] | .assets[] | .download_count' 2>/dev/null | awk '{s+=$1} END {print s+0}')"

# PR #1168 merged 2026-07-16 -> GitHub now detects the licence. Was NOASSERTION,
# which broke corporate licence scanners and deterred the exact commercial-
# exception buyers. If this ever reads NOASSERTION again, something re-prepended
# content to LICENSE — it must stay verbatim canonical AGPL-3.0 text and nothing
# else. The plain-language notice lives in LICENSE-NOTICE.md.
check "licence detected by GitHub" "AGPL-3.0" \
  "$(gh api repos/debpalash/OmniVoice-Studio/license --jq '.license.spdx_id' 2>/dev/null)"

# O1: the revenue door. "coming soon" = still shut. The notice lives in LICENSE
# until PR #1168 merges, then in LICENSE-NOTICE.md — check both, so this keeps
# working across the merge instead of silently GAPping.
lic_txt=$(curl -s --max-time 15 \
  https://raw.githubusercontent.com/debpalash/OmniVoice-Studio/main/LICENSE-NOTICE.md 2>/dev/null)
case "$lic_txt" in
  *"404: Not Found"*|"") lic_txt=$(curl -s --max-time 15 \
    https://raw.githubusercontent.com/debpalash/OmniVoice-Studio/main/LICENSE 2>/dev/null) ;;
esac
if [ -z "$lic_txt" ]; then
  check "commercial licence still 'coming soon'" "yes" "GAP"
else
  case "$lic_txt" in
    *"coming soon"*|*"Coming soon"*) v=yes ;;
    *) v=no ;;
  esac
  check "commercial licence still 'coming soon'" "yes" "$v"
fi

echo

# ── lead-data ───────────────────────────────────────────────────────────────
echo "lead-data (Clay alternative)"
check "repo visibility" "private" \
  "$(gh api repos/debpalash/lead-data --jq .visibility 2>/dev/null)"
check "stars" "0" \
  "$(gh api repos/debpalash/lead-data --jq .stargazers_count 2>/dev/null)"

# The README's own quickstart. Anonymous — authenticated gh follows redirects
# and hides this. That is exactly how it stayed broken.
check "README clone URL (anon)" "404" \
  "$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
      https://api.github.com/repos/yupcha-internal/lead-data 2>/dev/null)"

echo

# ── ResuBird ────────────────────────────────────────────────────────────────
echo "ResuBird"
# The bare /pricing path is a SOFT 404 — HTTP 200 with a "Page Not Found" body,
# because it's an SPA. A status-code check reports a healthy page.
#
# But NOTHING links to it: pricing lives at /#pricing on the homepage and works,
# with checkout at console.yupcha.com. An earlier version of this script (and of
# GOAL.md) treated the soft 404 as a broken, linked checkout. It was not. That
# claim came from a research agent and was repeated without being checked —
# which is the exact failure this script exists to catch, so it is left in the
# comments rather than quietly deleted.
pricing_body=$(curl -s -L --max-time 15 -A 'Mozilla/5.0' https://resubird.com/pricing 2>/dev/null)
if [ -z "$pricing_body" ]; then
  check "bare /pricing is still a soft 404" "yes" "GAP"
else
  case "$pricing_body" in
    *"Page Not Found"*) v=yes ;;
    *) v=no ;;
  esac
  check "bare /pricing is still a soft 404 (nit)" "yes" "$v"
fi

# What actually matters: does the real pricing section exist, and is checkout up?
home_body=$(curl -s -L --max-time 15 -A 'Mozilla/5.0' https://resubird.com/ 2>/dev/null)
case "$home_body" in
  *'/#pricing'*) v=yes ;;
  "") v=GAP ;;
  *) v=no ;;
esac
check "homepage links to /#pricing (the real one)" "yes" "$v"

check "billing console reachable" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' -L --max-time 20 -A 'Mozilla/5.0' \
      'https://console.yupcha.com/org/work?tab=plans&product=resubird' 2>/dev/null)"
check "homepage" "200" \
  "$(curl -s -o /dev/null -w '%{http_code}' -L --max-time 15 \
      -A 'Mozilla/5.0' https://resubird.com/ 2>/dev/null)"

# Serves AI crawlers? Its whole channel is organic search.
bot=$(curl -s -o /dev/null -w '%{http_code}' -L --max-time 15 \
  -A 'Mozilla/5.0 (compatible; GPTBot/1.0)' https://resubird.com/ 2>/dev/null)
check "serves bot UA (GPTBot)" "403" "$bot"

echo

# ── The clock ───────────────────────────────────────────────────────────────
echo "Deadlines"
now=$(date -u +%s)
aiact=$(date -u -j -f "%Y-%m-%d" "2026-08-02" +%s 2>/dev/null || date -u -d "2026-08-02" +%s)
days=$(( (aiact - now) / 86400 ))
if [ "$days" -ge 0 ]; then
  printf '  %s!!  %s  EU AI Act Art. 50(2) applies in %s%s days%s (2026-08-02)\n' \
    "$c_yel" "$c_off" "$c_yel" "$days" "$c_off"
  printf '      %ssynthetic audio must be machine-readable marked%s\n' "$c_dim" "$c_off"
  printf '      %sAGPL does NOT exempt — Art. 2(12) carves out Art. 50%s\n' "$c_dim" "$c_off"
  printf '      %sopen gap: /v1/audio/speech (issue #1169)%s\n' "$c_dim" "$c_off"
else
  printf '  %s!!  %s  EU AI Act Art. 50(2) IS IN FORCE (since 2026-08-02)\n' "$c_red" "$c_off"
fi

echo
printf 'ok %s  ·  changed %s  ·  gaps %s\n' "$pass" "$fail" "$gap"
echo
if [ "$fail" -gt 0 ]; then
  printf '%schanged = GOAL.md §2 is stale on those lines. Update it, do not work around it.%s\n\n' "$c_dim" "$c_off"
fi

# Gaps are not failures. Never exit non-zero for a gap.
[ "$fail" -eq 0 ]
