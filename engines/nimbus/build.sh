#!/usr/bin/env bash
# Build the fcr-nimbus engine binary.
#
# Vendored nimbus-eth2 lives at engines/nimbus/nimbus-eth2 (git submodule
# pinned to a stable release commit). We reuse Nimbus's pinned Nim toolchain
# and dependency graph rather than relying on the system Nim, so the build is
# reproducible across machines that have only a working C toolchain + make.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
NIMBUS_DIR="${HERE}/nimbus-eth2"
OUT_DIR="${ROOT}/results"
OUT_BIN="${OUT_DIR}/fcr-nimbus"

if [ ! -d "${NIMBUS_DIR}" ]; then
  echo "error: ${NIMBUS_DIR} missing; run 'git submodule update --init --recursive'" >&2
  exit 1
fi

echo "[build] bootstrapping nimbus-eth2 build system (one-time, ~20-40 min cold)" >&2
(cd "${NIMBUS_DIR}" && make -j"$(sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)" update)

mkdir -p "${OUT_DIR}"

NIMBUS_ENV="${NIMBUS_DIR}/env.sh"
if [ ! -f "${NIMBUS_ENV}" ]; then
  echo "error: ${NIMBUS_ENV} missing" >&2
  exit 1
fi

# Activate Nimbus's pinned Nim toolchain + Nimble paths.
# shellcheck disable=SC1090
source "${NIMBUS_ENV}"

# We compile from inside the nimbus-eth2 tree so all the nim.cfg search paths
# and nimbus-build-system.paths resolve. The Nim source lives outside the
# tree; we reference it via absolute path.
cd "${NIMBUS_DIR}"

NIM_PARAMS=(
  c
  -d:release
  -d:fakeCrypto
  -d:disableMarchNative
  -d:chronicles_log_level=NOTICE
  -d:const_preset=mainnet
  --threads:on
  --mm:refc
  --passC:-fno-lto
  --passL:-fno-lto
  -o:"${OUT_BIN}"
)

echo "[build] compiling fcr-nimbus -> ${OUT_BIN}" >&2
nim "${NIM_PARAMS[@]}" "${HERE}/src/fcr_nimbus.nim"

echo "[build] done: $(file "${OUT_BIN}")" >&2
