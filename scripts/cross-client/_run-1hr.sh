#!/usr/bin/env bash
# 1-hour 5-engine cross-client smoke. Range [435000, 435010) = ~64 min.
# Each engine: own cache dir with era/ symlink to shared, parallel=1, warmup=5.
set -euo pipefail

cd "$(dirname "$0")/../.."
set -a; source .env; set +a

RANGE_START=435000
RANGE_END=435010
WARMUP=5
SHARED_ERA="$HOME/.cache/fcr-simulator/era"

LH_BIN=$(pwd)/results/fcr-lighthouse
NIMBUS_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-nimbus/results/fcr-nimbus
TEKU_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-teku/results/fcr-teku
LODESTAR_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-lodestar/results/fcr-lodestar
GRANDINE_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-grandine/results/fcr-grandine

run_engine() {
    local engine=$1 binary=$2
    local cache=/tmp/fcr-cache-1hr-${engine}
    local out=results/1hr/${engine}
    local log=/tmp/1hr-${engine}.log

    rm -rf "$cache" "$out"
    mkdir -p "$cache" "$out"
    ln -s "$SHARED_ERA" "$cache/era"

    echo "[${engine}] launching binary=$binary" > "$log"
    ./results/fcr-orchestrator \
        --engine "$engine" --engine-binary "$binary" \
        --network mainnet \
        --start-epoch "$RANGE_START" --end-epoch "$RANGE_END" \
        --warmup-epochs "$WARMUP" \
        --parallel 1 \
        --beacon-node-url "$BEACON_NODE_URL" \
        --output "${out}/run.csv" --output-format both \
        --attestation-source-mode next-non-missed \
        --lookahead-cap 4 \
        --cache-dir "$cache" \
        >> "$log" 2>&1
    echo "[${engine}] exit=$? at $(date +%H:%M:%S)" >> "$log"
}

run_engine lighthouse "$LH_BIN"       &
run_engine nimbus     "$NIMBUS_BIN"   &
run_engine teku       "$TEKU_BIN"     &
run_engine lodestar   "$LODESTAR_BIN" &
run_engine grandine   "$GRANDINE_BIN" &

wait
echo "1HR DONE at $(date)"
