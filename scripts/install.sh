#!/usr/bin/env bash
# Install OpenGTM.
#
# Binary mode: download the `opengtm` release binary for this OS and CPU, verify
# it against the release's SHA-256 checksums and (when cosign is installed) the
# keyless Sigstore signature on those checksums, and install it. With
# --quickstart it then runs `opengtm init` and starts the stack with Docker
# Compose.
#
#   curl -fsSL https://github.com/debpalash/OpenGTM/releases/latest/download/install.sh | bash -s -- --quickstart
#
# Source mode: build the Docker Compose stack from a repository checkout (the
# behaviour this script always had). It is chosen automatically when the script
# runs from inside a checkout, so `./scripts/install.sh` keeps working as
# documented; pass --binary to install the release binary from a checkout.
set -euo pipefail

REPO="${OPENGTM_REPO:-debpalash/OpenGTM}"
VERSION="${OPENGTM_VERSION:-latest}"
# Override to install from a mirror or a local directory (file:///path): the
# directory must hold the release assets (archives, checksums.txt, signature).
BASE_URL="${OPENGTM_RELEASE_BASE_URL:-}"
BIN_DIR="${OPENGTM_BIN_DIR:-}"
INSTALL_DIR="${OPENGTM_INSTALL_DIR:-$PWD/opengtm}"
PROFILE="lite"
QUICKSTART=0
MODE=auto   # auto | binary | source
START_STACK=1
REQUIRE_SIGNATURE="${OPENGTM_REQUIRE_SIGNATURE:-0}"
REPOSITORY_URL="${OPENGTM_REPOSITORY_URL:-https://github.com/${REPO}.git}"
# The identity every release is signed with: the release workflow of this
# repository, on a version tag.
SIGNER_IDENTITY_REGEXP="${OPENGTM_SIGNER_IDENTITY_REGEXP:-^https://github.com/${REPO}/\\.github/workflows/release\\.yml@refs/tags/v.+\$}"
OIDC_ISSUER="https://token.actions.githubusercontent.com"

usage() {
  cat <<'EOF'
Install OpenGTM.

Usage: install.sh [options]

  --version VERSION     release to install, e.g. 3.1.0 (default: latest)
  --bin-dir PATH        where to put the binary (default: /usr/local/bin if
                        writable, otherwise ~/.local/bin)
  --quickstart          after installing, run `opengtm init --yes --start`
  --profile NAME        profile for --quickstart: lite, standard or full (default lite)
  --install-dir PATH    install directory for --quickstart (default ./opengtm)
  --require-signature   fail unless the cosign signature verifies (needs cosign)
  --binary              install the release binary even when run from a checkout
  --from-source         build the Compose stack from a checkout (cloned if needed)
  --no-start            source mode: configure but do not start (implies --from-source)

Run from a repository checkout, or with --from-source or --no-start, the script
builds the Compose stack from source as it always did. Anywhere else (for
example piped from curl) it installs the verified release binary.

Environment: OPENGTM_VERSION, OPENGTM_BIN_DIR, OPENGTM_INSTALL_DIR,
OPENGTM_REQUIRE_SIGNATURE=1, OPENGTM_RELEASE_BASE_URL, OPENGTM_REPOSITORY_URL.
EOF
}

die() { echo "install.sh: $*" >&2; exit 1; }
say() { echo "==> $*"; }

while (($#)); do
  case "$1" in
    --version) VERSION="${2:?--version requires a value}"; shift 2 ;;
    --bin-dir) BIN_DIR="${2:?--bin-dir requires a path}"; shift 2 ;;
    --dir|--install-dir) INSTALL_DIR="${2:?$1 requires a path}"; shift 2 ;;
    --profile) PROFILE="${2:?--profile requires a value}"; shift 2 ;;
    --quickstart) QUICKSTART=1; shift ;;
    --require-signature) REQUIRE_SIGNATURE=1; shift ;;
    --binary) MODE=binary; shift ;;
    --from-source) MODE=source; shift ;;
    --no-start) START_STACK=0; [[ "$MODE" == auto ]] && MODE=source; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

# ── from source (previous behaviour) ───────────────────────────────────────
random_hex() {
  if command -v openssl >/dev/null; then
    openssl rand -hex "$1"
  else
    LC_ALL=C od -An -N "$1" -tx1 /dev/urandom | tr -d ' \n'
  fi
}

install_from_source() {
  command -v git >/dev/null || die "git is required"
  command -v docker >/dev/null || die "Docker is required: https://docs.docker.com/get-docker/"
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required"

  local script_dir candidate_root install_root
  script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)"
  candidate_root="$(cd -- "$script_dir/.." 2>/dev/null && pwd || true)"
  if [[ -f "$candidate_root/docker-compose.yml" && -f "$candidate_root/.env.example" ]]; then
    install_root="$candidate_root"
  elif [[ -f "$PWD/docker-compose.yml" && -f "$PWD/.env.example" ]]; then
    install_root="$PWD"
  elif [[ -f "$INSTALL_DIR/docker-compose.yml" && -f "$INSTALL_DIR/.env.example" ]]; then
    install_root="$(cd -- "$INSTALL_DIR" && pwd)"
  elif [[ -e "$INSTALL_DIR" ]]; then
    die "Install path already exists and is not an OpenGTM checkout: $INSTALL_DIR"
  else
    git clone --depth 1 "$REPOSITORY_URL" "$INSTALL_DIR"
    install_root="$(cd -- "$INSTALL_DIR" && pwd)"
  fi

  cd "$install_root"
  if [[ ! -d data ]]; then
    mkdir data
    # Containers use UID 1000 when installed by root; keep the bind mount writable.
    if ((EUID == 0)); then chown 1000:1000 data; fi
  fi

  if [[ ! -f .env ]]; then
    umask 077
    local postgres_password runtime_password admin_password secret_key app_uid app_gid
    postgres_password="$(random_hex 24)"
    runtime_password="$(random_hex 24)"
    admin_password="$(random_hex 12)"
    secret_key="$(random_hex 48)"
    app_uid="$(id -u)"
    app_gid="$(id -g)"
    if ((app_uid == 0)); then app_uid=1000; app_gid=1000; fi
    cp .env.example .env
    {
      printf '\n# Generated by scripts/install.sh\n'
      printf 'APP_ENV=local\n'
      printf 'SECRET_KEY=%s\n' "$secret_key"
      printf 'POSTGRES_USER=yupcha\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=yupcha\n' "$postgres_password"
      printf 'DATABASE_URL=postgresql+psycopg://yupcha:%s@postgres:5432/yupcha\n' "$postgres_password"
      printf 'YUPCHA_RUNTIME_DB_USER=yupcha_runtime\nYUPCHA_RUNTIME_DB_PASSWORD=%s\n' "$runtime_password"
      printf 'APP_DATABASE_URL=postgresql+psycopg://yupcha_runtime:%s@postgres:5432/yupcha\n' "$runtime_password"
      printf 'SEED_ADMIN_USERNAME=admin\nSEED_ADMIN_PASSWORD=%s\n' "$admin_password"
      printf 'APP_UID=%s\nAPP_GID=%s\n' "$app_uid" "$app_gid"
    } >> .env
    local credentials_file="$install_root/.opengtm-initial-credentials"
    printf 'URL=http://localhost:3000\nUSERNAME=admin\nPASSWORD=%s\n' "$admin_password" > "$credentials_file"
    echo "Created .env and one-time credentials at $credentials_file"
  else
    echo "Keeping existing $install_root/.env"
  fi

  if ((START_STACK)); then
    docker compose up -d --build
    docker compose ps
    echo "OpenGTM is starting at http://localhost:3000"
  else
    echo "Configured $install_root; run: cd '$install_root' && docker compose up -d --build"
  fi
}

if [[ "$MODE" == auto ]]; then
  # Inside a checkout the historical behaviour is kept; piped from curl there is
  # no checkout, so the release binary is installed.
  here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]:-$0}")/.." 2>/dev/null && pwd || true)"
  if [[ -n "$here" && -f "$here/docker-compose.yml" && -f "$here/.env.example" ]]; then
    MODE=source
  else
    MODE=binary
  fi
fi
if [[ "$MODE" == source ]]; then
  install_from_source
  exit 0
fi

# ── release binary ──────────────────────────────────────────────────────────
command -v curl >/dev/null || die "curl is required"
command -v tar >/dev/null || die "tar is required"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s); on Windows use scripts/install.ps1 or a release .zip" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported CPU $(uname -m) (releases cover amd64 and arm64)" ;;
esac

sha256_of() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null; then shasum -a 256 "$1" | cut -d' ' -f1
  else die "sha256sum or shasum is required to verify the download"; fi
}

if [[ "$VERSION" == "latest" ]]; then
  if [[ -n "$BASE_URL" ]]; then
    die "set --version when using OPENGTM_RELEASE_BASE_URL"
  fi
  # /releases/latest redirects to /releases/tag/vX.Y.Z; read where it lands.
  final="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")" \
    || die "could not look up the latest release"
  VERSION="${final##*/}"
fi
VERSION="${VERSION#v}"
[[ "$VERSION" =~ ^[0-9][0-9A-Za-z.+-]*$ ]] || die "invalid version '$VERSION'"

asset="opengtm_${VERSION}_${os}_${arch}.tar.gz"
base="${BASE_URL:-https://github.com/${REPO}/releases/download/v${VERSION}}"
base="${base%/}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

say "downloading opengtm ${VERSION} for ${os}/${arch}"
fetch() { curl -fsSL --retry 3 -o "$2" "$base/$1" || die "download failed: $base/$1"; }
fetch "$asset" "$tmp/$asset"
fetch checksums.txt "$tmp/checksums.txt"

# 1. Checksum: the archive must match its line in checksums.txt.
expected="$(awk -v f="$asset" '$2 == f {print $1}' "$tmp/checksums.txt")"
[[ -n "$expected" ]] || die "checksums.txt has no entry for $asset"
actual="$(sha256_of "$tmp/$asset")"
[[ "$expected" == "$actual" ]] || die "checksum mismatch for $asset (expected $expected, got $actual); refusing to install"
say "sha256 verified"

# 2. Signature: checksums.txt must be signed by this repository's release
# workflow. The checksum alone only proves the download matches a file from the
# same origin; the signature proves who built it.
bundle="$tmp/checksums.txt.sigstore.json"
if command -v cosign >/dev/null; then
  if curl -fsSL --retry 3 -o "$bundle" "$base/checksums.txt.sigstore.json"; then
    cosign verify-blob \
      --bundle "$bundle" \
      --certificate-identity-regexp "$SIGNER_IDENTITY_REGEXP" \
      --certificate-oidc-issuer "$OIDC_ISSUER" \
      "$tmp/checksums.txt" >/dev/null 2>&1 \
      || die "cosign signature verification FAILED for checksums.txt; refusing to install"
    say "cosign signature verified (signed by the ${REPO} release workflow)"
  elif [[ "$REQUIRE_SIGNATURE" == "1" ]]; then
    die "no signature bundle published for this release and OPENGTM_REQUIRE_SIGNATURE=1"
  else
    echo "warning: no signature bundle found at $base/checksums.txt.sigstore.json; only the checksum was verified" >&2
  fi
elif [[ "$REQUIRE_SIGNATURE" == "1" ]]; then
  die "--require-signature needs cosign: https://docs.sigstore.dev/cosign/system_config/installation/"
else
  echo "warning: cosign is not installed, so only the SHA-256 checksum was verified." >&2
  echo "         Install cosign and re-run (or pass --require-signature) to verify the release signature." >&2
fi

tar -xzf "$tmp/$asset" -C "$tmp" opengtm || die "archive does not contain the opengtm binary"

if [[ -z "$BIN_DIR" ]]; then
  if [[ -w /usr/local/bin ]]; then BIN_DIR=/usr/local/bin; else BIN_DIR="$HOME/.local/bin"; fi
fi
mkdir -p "$BIN_DIR"
install -m 0755 "$tmp/opengtm" "$BIN_DIR/opengtm"
say "installed $BIN_DIR/opengtm ($("$BIN_DIR/opengtm" version))"
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "note: $BIN_DIR is not on your PATH" >&2 ;; esac

if ((QUICKSTART)); then
  command -v docker >/dev/null || die "Docker is required for --quickstart: https://docs.docker.com/get-docker/"
  docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required for --quickstart"
  say "initialising $INSTALL_DIR (profile $PROFILE) and starting the stack"
  "$BIN_DIR/opengtm" init --dir "$INSTALL_DIR" --profile "$PROFILE" --yes --start
else
  echo "Next: opengtm init --profile lite --start   (or run with --quickstart)"
fi
