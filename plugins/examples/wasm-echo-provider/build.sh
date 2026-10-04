#!/usr/bin/env bash
# Rebuild the committed example module used by the Go wasmhost tests and
# refresh the sha256 pinned in plugin.yaml.
#
#   rustup target add wasm32-unknown-unknown   # once
#   ./build.sh
set -euo pipefail
cd "$(dirname "$0")"
# Remap local paths so the module is reproducible and leaks no home directory.
cargo_home="${CARGO_HOME:-$HOME/.cargo}"
export RUSTFLAGS="--remap-path-prefix=${cargo_home}=/cargo --remap-path-prefix=$(pwd)=/src ${RUSTFLAGS:-}"
cargo build --locked --release --target wasm32-unknown-unknown
cp target/wasm32-unknown-unknown/release/wasm_echo_provider.wasm plugin.wasm
sum=$(sha256sum plugin.wasm | cut -d' ' -f1)
sed -i.bak -E "s/^(  sha256: ).*/\1\"${sum}\"/" plugin.yaml && rm -f plugin.yaml.bak
ls -l plugin.wasm
echo "sha256 ${sum}"
