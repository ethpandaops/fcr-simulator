#!/usr/bin/env bash
# Internal: 12-hour cross-client run across all 5 engines.
# Each engine gets its own top-level --cache-dir under /tmp/fcr-cache-12hr-<engine>/
# with a symlink era/ -> the shared ERA cache, so ERA bytes aren't duplicated
# but worker-*.jsonl can't collide.
set -euo pipefail

cd "$(dirname "$0")/../.."
set -a; source .env; set +a

RANGE_START=435000
RANGE_END=435113   # 113 epochs ≈ 12 mainnet hours
WARMUP=5
PARALLEL=2        # workers per engine inside a chunk
SHARED_ERA="$HOME/.cache/fcr-simulator/era"

LH_BIN=$(pwd)/results/fcr-lighthouse
NIMBUS_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-nimbus/results/fcr-nimbus
TEKU_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-teku/results/fcr-teku
LODESTAR_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-lodestar/results/fcr-lodestar
GRANDINE_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-grandine/results/fcr-grandine

run_engine() {
    local engine=$1 binary=$2
    local cache=/tmp/fcr-cache-12hr-${engine}
    local out=results/12hr/${engine}
    local log=/tmp/12hr-${engine}.log

    rm -rf "$cache"
    mkdir -p "$cache" "$out"
    ln -s "$SHARED_ERA" "$cache/era"

    echo "[${engine}] launching binary=$binary cache=$cache" >> "$log"
    ./results/fcr-orchestrator \
        --engine "$engine" --engine-binary "$binary" \
        --network mainnet \
        --start-epoch "$RANGE_START" --end-epoch "$RANGE_END" \
        --warmup-epochs "$WARMUP" \
        --parallel "$PARALLEL" \
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
echo "ALL ENGINES DONE at $(date)"
