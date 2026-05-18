#!/usr/bin/env bash
# Internal: run remaining smoke epochs for nimbus/teku/lodestar in parallel.
set -euo pipefail

cd "$(dirname "$0")/../.."
set -a; source .env; set +a

ORCH=$(pwd)/results/fcr-orchestrator
NIMBUS_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-nimbus/results/fcr-nimbus
TEKU_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-teku/results/fcr-teku
LODESTAR_BIN=/Users/samcm/go/src/github.com/ethpandaops/fcr-simulator/.claude/worktrees/engine-lodestar/results/fcr-lodestar

run_engine() {
    local engine=$1
    local binary=$2
    shift 2
    local epochs=("$@")
    local logfile=/tmp/smoke-${engine}.log
    : > "$logfile"
    echo "[${engine}] launching ${#epochs[@]} epochs at $(date +%H:%M:%S)" >> "$logfile"
    mkdir -p "results/cross-client/${engine}"
    for ep in "${epochs[@]}"; do
        local outfile="results/cross-client/${engine}/epoch-${ep}.csv"
        if [[ -f "$outfile" ]]; then
            echo "[${engine}] epoch ${ep} already exists, skipping" >> "$logfile"
            continue
        fi
        echo "[${engine}] epoch ${ep} starting at $(date +%H:%M:%S)" >> "$logfile"
        "$ORCH" \
            --engine "$engine" --engine-binary "$binary" \
            --network mainnet --start-epoch "$ep" --end-epoch "$((ep+1))" \
            --warmup-epochs 2 --parallel 1 \
            --beacon-node-url "$BEACON_NODE_URL" \
            --output "$outfile" --output-format both \
            --cache-dir "/tmp/fcr-cache-${engine}-${ep}" >> "/tmp/smoke-${engine}-ep-${ep}.log" 2>&1
        echo "[${engine}] epoch ${ep} exit=$? at $(date +%H:%M:%S)" >> "$logfile"
    done
    echo "[${engine}] complete at $(date +%H:%M:%S)" >> "$logfile"
}

run_engine nimbus   "$NIMBUS_BIN"   441579 442318 444131 444792 &
NIMBUS_PID=$!
run_engine teku     "$TEKU_BIN"     437504 437958 438580 439782 440350 441579 442318 444131 444792 &
TEKU_PID=$!
run_engine lodestar "$LODESTAR_BIN" 437504 437958 438580 439782 440350 441579 442318 444131 444792 &
LODESTAR_PID=$!

wait "$NIMBUS_PID"   && echo "nimbus done"   || echo "nimbus failed"
wait "$TEKU_PID"     && echo "teku done"     || echo "teku failed"
wait "$LODESTAR_PID" && echo "lodestar done" || echo "lodestar failed"
echo "ALL DONE at $(date +%H:%M:%S)"
