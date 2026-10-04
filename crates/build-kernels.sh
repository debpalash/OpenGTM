#!/usr/bin/env bash
# Build the Rust kernels as a WebAssembly Extism plugin and copy the artifact
# into the Go server package that embeds it. The .wasm is committed so that
# `go build` works without a Rust toolchain; rerun this script after changing
# anything under crates/ and commit the updated artifact.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/.." && pwd)"
target=wasm32-unknown-unknown
out="$repo/apps/server/internal/kernels/opengtm_kernels.wasm"

# Remap local paths (panic locations survive `strip`) so the artifact does not
# depend on where the repository or the cargo registry lives.
cargo_home="${CARGO_HOME:-$HOME/.cargo}"
export RUSTFLAGS="${RUSTFLAGS:-} --remap-path-prefix=$cargo_home/registry/src=/cargo/registry/src --remap-path-prefix=$here=/opengtm/crates"

cargo build --manifest-path "$here/Cargo.toml" -p opengtm-kernels --release --target "$target" --locked
wasm="$here/target/$target/release/opengtm_kernels.wasm"

if command -v wasm-opt >/dev/null 2>&1; then
  wasm-opt -Oz --enable-bulk-memory --enable-sign-ext --enable-nontrapping-float-to-int \
    --enable-mutable-globals "$wasm" -o "$wasm.opt"
  mv "$wasm.opt" "$wasm"
  echo "wasm-opt -Oz applied"
else
  echo "wasm-opt not found; skipping -Oz pass"
fi

cp "$wasm" "$out"
size=$(wc -c <"$out" | tr -d ' ')
sha=$(sha256sum "$out" 2>/dev/null || shasum -a 256 "$out")
echo "wrote ${out#"$repo"/}"
echo "size:   $size bytes"
echo "sha256: ${sha%% *}"
