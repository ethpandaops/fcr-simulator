#!/usr/bin/env bash
# Build the Prysm-backed FCR simulator adapter.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
PRYSM_DIR="${HERE}/prysm"
PACKAGE_DIR="${PRYSM_DIR}/testing/spectest/shared/common/forkchoice"
ADAPTER_DST="${PACKAGE_DIR}/fcr_simulator_adapter_test.go"
BRIDGE_DST="${PRYSM_DIR}/beacon-chain/blockchain/fcr_simulator_bridge.go"
OUT="${ROOT}/results/fcr-prysm"

if [ ! -f "${PRYSM_DIR}/go.mod" ]; then
  echo "prysm submodule missing at ${PRYSM_DIR}; run: git submodule update --init --recursive" >&2
  exit 1
fi

cleanup() {
  rm -f "${ADAPTER_DST}" "${BRIDGE_DST}"
}
trap cleanup EXIT

cp "${HERE}/fcr_simulator_adapter_test.go" "${ADAPTER_DST}"
cp "${HERE}/fcr_simulator_bridge.go" "${BRIDGE_DST}"
mkdir -p "$(dirname "${OUT}")"

echo "[build] compiling Prysm FCR adapter -> ${OUT}" >&2
(
  cd "${PRYSM_DIR}"
  GOTOOLCHAIN=auto go test -c -o "${OUT}" ./testing/spectest/shared/common/forkchoice
)

echo "[build] done: ${OUT}" >&2
