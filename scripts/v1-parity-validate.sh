#!/usr/bin/env bash
# V1 parity validation: orchestrator + slim Lighthouse engine vs. current in-process simulator.
#
# Goal: prove the new orchestrator-engine architecture produces functionally identical
# FCR results to the old in-process simulator on the same range. "Functionally identical"
# means agreement on every consensus-meaningful column; we project out:
#   - fcr_eval_duration_us  (wall-clock, nondeterministic)
#   - source_block_slot     (new-only column; recoverable from has_block + plan)
#   - attestation_source    (old-only column; orchestrator owns this now)
#   - eval_slot             (old-only column; always slot+1, redundant)
#   - engine_*, schema_*, attestation_source_mode, lookahead_cap  (orchestrator-added metadata)
#
# Compared columns (must be byte-equal):
#   slot, epoch, has_block, block_root, head_root, confirmed_root, confirmed_slot,
#   confirmation_delay_slots, fast_confirmed, strict_one_slot_confirmed,
#   finalized_epoch, justified_epoch, num_attestations_injected,
#   is_epoch_boundary, is_missed_slot
#
# Usage:
#   BN_URL=http://samcm-nuc14-1:5052 ./scripts/v1-parity-validate.sh
#
# Defaults to BN_URL=http://samcm-nuc14-1:5052 (Lighthouse) — same-client comparison
# avoids cross-client noise.

set -euo pipefail

BN_URL="${BN_URL:-http://samcm-nuc14-1:5052}"
START_EPOCH="${START_EPOCH:-435000}"
END_EPOCH="${END_EPOCH:-435010}"
WARMUP_EPOCHS="${WARMUP_EPOCHS:-10}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$REPO_ROOT/results"
mkdir -p "$OUT"

echo "=== Build orchestrator (Go) ==="
( cd "$REPO_ROOT" && go build -o "$OUT/fcr-orchestrator" ./cmd/fcr-orchestrator )

echo "=== Build slim Lighthouse engine (Rust, release, fake_crypto) ==="
echo "    This takes 10-20 min on first build; subsequent are incremental."
( cd "$REPO_ROOT/lighthouse" && \
  CARGO_NET_GIT_FETCH_WITH_CLI=true \
  cargo build -p fcr-simulator --features fake_crypto --release )
cp "$REPO_ROOT/lighthouse/target/release/fcr-lighthouse" "$OUT/fcr-lighthouse"

echo "=== Run new orchestrator+engine ==="
"$OUT/fcr-orchestrator" \
  --engine lighthouse \
  --engine-binary "$OUT/fcr-lighthouse" \
  --network mainnet \
  --start-epoch "$START_EPOCH" \
  --end-epoch "$END_EPOCH" \
  --warmup-epochs "$WARMUP_EPOCHS" \
  --parallel 1 \
  --beacon-node-url "$BN_URL" \
  --output "$OUT/v1-parity-new.csv" \
  --output-format both \
  --attestation-source-mode next-non-missed \
  --lookahead-cap 4

echo ""
echo "=== NEW output written ==="
ls -la "$OUT/v1-parity-new"*

echo ""
echo "=== OLD reference output ==="
echo "To complete parity validation, run the OLD simulator (from main branch) on the same range:"
echo "  git worktree add /tmp/fcr-old main && cd /tmp/fcr-old"
echo "  cd lighthouse && cargo build -p fcr-simulator --features fake_crypto --release"
echo "  ./target/release/fcr-simulator \\"
echo "    --beacon-node-url $BN_URL \\"
echo "    --start-epoch $START_EPOCH --end-epoch $END_EPOCH \\"
echo "    --warmup-epochs $WARMUP_EPOCHS --parallel 1 \\"
echo "    --output $OUT/v1-parity-old.csv"
echo ""
echo "Then run the diff:"
echo "  $REPO_ROOT/scripts/v1-parity-diff.sh $OUT/v1-parity-old.csv $OUT/v1-parity-new.csv"
