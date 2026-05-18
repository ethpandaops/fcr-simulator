#!/usr/bin/env bash
# Build the fcr-lighthouse engine.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"
LH_DIR="${HERE}/lighthouse"
OUT="${REPO_ROOT}/results/fcr-lighthouse"

if [ ! -d "${LH_DIR}" ]; then
  echo "lighthouse submodule missing at ${LH_DIR}; run: git submodule update --init --recursive" >&2
  exit 1
fi

cd "${LH_DIR}"
CARGO_NET_GIT_FETCH_WITH_CLI=true cargo build -p fcr-simulator --features fake_crypto --release

mkdir -p "$(/usr/bin/dirname "${OUT}")"
/bin/cp "target/release/fcr-lighthouse" "${OUT}"
echo "Built ${OUT}"
