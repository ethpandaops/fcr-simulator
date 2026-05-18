#!/usr/bin/env bash
# Run the orchestrator over the 10 deterministic smoke-test epochs for one engine,
# writing results to results/cross-client/<engine>/epoch-<N>.{jsonl,csv,manifest.json}.
#
# Usage:
#   scripts/cross-client/run.sh <engine> [engine-binary]
#
# engine-binary defaults to ./results/fcr-<engine>; orchestrator auto-builds if missing.
#
# Requires:
#   - ./results/fcr-orchestrator on disk
#   - BN_URL in .env (an archive-mode mainnet beacon node)

set -euo pipefail

ENGINE="${1:?engine name required (e.g. lighthouse, teku, lodestar, nimbus, prysm, grandine)}"
ENGINE_BINARY="${2:-}"

ORCHESTRATOR="${ORCHESTRATOR:-./results/fcr-orchestrator}"
OUTPUT_ROOT="${OUTPUT_ROOT:-./results/cross-client}"
CACHE_DIR="${CACHE_DIR:-$HOME/.cache/fcr-simulator}"
WARMUP_EPOCHS="${WARMUP_EPOCHS:-10}"
PARALLEL="${PARALLEL:-1}"
ATT_MODE="${ATT_MODE:-next-non-missed}"
LOOKAHEAD_CAP="${LOOKAHEAD_CAP:-4}"

if [[ ! -x "$ORCHESTRATOR" ]]; then
    echo "missing orchestrator at $ORCHESTRATOR; build with: go build -o $ORCHESTRATOR ./cmd/fcr-orchestrator" >&2
    exit 2
fi
if [[ -f .env ]]; then
    set -a; source .env; set +a
fi
BN="${BEACON_NODE_URL:-${BN_URL:-}}"
if [[ -z "$BN" ]]; then
    echo "BEACON_NODE_URL (or BN_URL) must be set in .env or environment" >&2
    exit 2
fi

ENGINE_OUT="$OUTPUT_ROOT/$ENGINE"
mkdir -p "$ENGINE_OUT"

EPOCHS=$(python3 scripts/cross-client/pick-epochs.py)

PASS=0
FAIL=0
for ep in $EPOCHS; do
    out_base="$ENGINE_OUT/epoch-$ep"
    echo "[$ENGINE] epoch $ep -> $out_base.csv"
    if "$ORCHESTRATOR" \
            --engine "$ENGINE" \
            ${ENGINE_BINARY:+--engine-binary "$ENGINE_BINARY"} \
            --network mainnet \
            --start-epoch "$ep" \
            --end-epoch "$((ep + 1))" \
            --warmup-epochs "$WARMUP_EPOCHS" \
            --parallel "$PARALLEL" \
            --beacon-node-url "$BN" \
            --output "$out_base.csv" \
            --output-format both \
            --attestation-source-mode "$ATT_MODE" \
            --lookahead-cap "$LOOKAHEAD_CAP" \
            --cache-dir "$CACHE_DIR"; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "  FAILED epoch $ep" >&2
    fi
done

echo ""
echo "[$ENGINE] $PASS/$((PASS + FAIL)) epochs succeeded"
exit $(( FAIL > 0 ? 1 : 0 ))
