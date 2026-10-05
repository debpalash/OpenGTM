#!/usr/bin/env bash
# Tests scripts/install.sh against a local release directory (as produced by
# `goreleaser release --snapshot`): a good install, a tampered archive, a
# missing checksum entry, and each cosign outcome. No network and no real
# signature is needed: cosign is a stub that records its arguments, plus the
# real cosign (if present) to prove it rejects a forged bundle.
#
#   scripts/packaging/test-install.sh DIST_DIR
set -euo pipefail

DIST="${1:?usage: test-install.sh DIST_DIR (a directory with opengtm_*.tar.gz and checksums.txt)}"
INSTALL="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/install.sh"
DIST="$(cd "$DIST" && pwd)"

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "unsupported OS"; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported CPU"; exit 1 ;; esac

archive="$(cd "$DIST" && find . -maxdepth 1 -name "opengtm_*_${os}_${arch}.tar.gz" | head -n 1)"
archive="${archive#./}"
[[ -n "$archive" ]] || { echo "no opengtm_*_${os}_${arch}.tar.gz in $DIST"; exit 1; }
VERSION="${archive#opengtm_}"; VERSION="${VERSION%_"${os}"_"${arch}".tar.gz}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
pass=0
ok() { pass=$((pass + 1)); echo "ok   $*"; }
bad() { echo "FAIL $*" >&2; exit 1; }

# Fresh copies of the release so tests can damage them.
release() { rm -rf "${WORK:?}/${1:?}" && mkdir -p "$WORK/$1" && cp "$DIST"/*.tar.gz "$DIST"/checksums.txt "$WORK/$1/"; }
run_install() { # release-dir bin-dir extra-args...
  local rel="$1" bin="$2"; shift 2
  OPENGTM_RELEASE_BASE_URL="file://$WORK/$rel" "$INSTALL" --binary --version "$VERSION" --bin-dir "$bin" "$@"
}
stub_cosign() { # dir exit-code
  mkdir -p "$1"
  printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$@" > "%s/args"\nexit %s\n' "$1" "$2" > "$1/cosign"
  chmod +x "$1/cosign"
}

# 1. Good install with the checksum verified; the binary runs.
release good
run_install good "$WORK/bin1" >"$WORK/out1" 2>&1 || { cat "$WORK/out1"; bad "good install failed"; }
[[ "$("$WORK/bin1/opengtm" version)" == "$VERSION" ]] || bad "installed binary reports the wrong version"
grep -q "sha256 verified" "$WORK/out1" || bad "checksum verification not reported"
ok "install verifies the checksum and installs a working binary"

# 2. A tampered archive is refused and nothing is installed.
release tampered
printf 'x' >> "$WORK/tampered/$archive"
if run_install tampered "$WORK/bin2" >"$WORK/out2" 2>&1; then bad "tampered archive was installed"; fi
grep -q "checksum mismatch" "$WORK/out2" || bad "no mismatch message: $(cat "$WORK/out2")"
[[ ! -e "$WORK/bin2/opengtm" ]] || bad "binary installed despite the mismatch"
ok "a tampered archive is refused"

# 3. An archive missing from checksums.txt is refused.
release unlisted
grep -v " $archive\$" "$WORK/unlisted/checksums.txt" > "$WORK/unlisted/checksums.new"
mv "$WORK/unlisted/checksums.new" "$WORK/unlisted/checksums.txt"
if run_install unlisted "$WORK/bin3" >"$WORK/out3" 2>&1; then bad "unlisted archive was installed"; fi
ok "an archive absent from checksums.txt is refused"

# 4. --require-signature without cosign fails closed (skipped if cosign is installed).
if command -v cosign >/dev/null; then
  echo "skip --require-signature without cosign (cosign is installed)"
else
  release sigreq
  if run_install sigreq "$WORK/bin4" --require-signature >"$WORK/out4" 2>&1; then bad "--require-signature passed without cosign"; fi
  [[ ! -e "$WORK/bin4/opengtm" ]] || bad "binary installed although the signature could not be checked"
  ok "--require-signature fails closed when the signature cannot be checked"
fi

# 5. A cosign that rejects the signature blocks the install.
release badsig
echo '{}' > "$WORK/badsig/checksums.txt.sigstore.json"
stub_cosign "$WORK/stub-fail" 1
if PATH="$WORK/stub-fail:$PATH" run_install badsig "$WORK/bin5" >"$WORK/out5" 2>&1; then bad "install proceeded after cosign failed"; fi
grep -q "signature verification FAILED" "$WORK/out5" || bad "no signature failure message"
[[ ! -e "$WORK/bin5/opengtm" ]] || bad "binary installed despite a failed signature"
ok "a failed cosign verification blocks the install"

# 6. A cosign that accepts it is called with the pinned identity and issuer.
release goodsig
echo '{}' > "$WORK/goodsig/checksums.txt.sigstore.json"
stub_cosign "$WORK/stub-ok" 0
PATH="$WORK/stub-ok:$PATH" run_install goodsig "$WORK/bin6" --require-signature >"$WORK/out6" 2>&1 || { cat "$WORK/out6"; bad "install failed with a passing cosign"; }
args="$(cat "$WORK/stub-ok/args")"
for want in verify-blob --bundle --certificate-identity-regexp "release\\.yml@refs/tags/v" \
            --certificate-oidc-issuer https://token.actions.githubusercontent.com; do
  grep -qF -- "$want" <<<"$args" || bad "cosign was not given $want: $args"
done
ok "cosign is invoked with the release workflow identity and the GitHub OIDC issuer"

# 7. The real cosign rejects a forged bundle (skipped when it is not installed).
if command -v cosign >/dev/null; then
  release forged
  echo '{"not":"a bundle"}' > "$WORK/forged/checksums.txt.sigstore.json"
  if run_install forged "$WORK/bin7" >"$WORK/out7" 2>&1; then bad "forged signature accepted by real cosign"; fi
  ok "the real cosign rejects a forged bundle"
else
  echo "skip real cosign test (cosign not installed)"
fi

echo "install.sh: $pass checks passed"
