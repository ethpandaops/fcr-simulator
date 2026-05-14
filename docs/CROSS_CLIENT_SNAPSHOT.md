# Cross-client smoke snapshot — epoch 435432

First validated cross-engine FCR comparison. Three engines ran the same orchestrator-served chain at epoch 435432 (32 slots) under identical conditions: `--warmup-epochs 2`, `--attestation-source-mode next-non-missed`, `--lookahead-cap 4`.

| Engine | Version | Commit | Records |
|---|---|---|---|
| nimbus | v26.5.0 | `6fb05f36804d53c2e8e014cfeeea8ad7996a5efe` | 32 |
| teku | develop | `c5825d53325cd67ab91b35cc544a7b660be317ff` | 32 |
| lodestar | v0.39.0-3198-g549b4c390c | `549b4c390cd5bb6d2bb35c48f59e7a04575ba496` | 32 |

## Consensus results (per `scripts/cross-client/diff.py`)

100% agreement across all three engines on:

- `has_block`
- `block_root`
- `head_root`
- `source_block_slot`
- `is_epoch_boundary`
- `is_missed_slot`

30/32 slots agree on `confirmed_root`. Two slots diverge:

| Slot | nimbus / lodestar | teku |
|---|---|---|
| 13933826 | `0x0ee9606c3bf65721…` | `0x626ea8454326ab79…` |
| 13933828 | `0x188b9253317e7614…` | `0x7e224a518bfbf898…` |

Teku reports the next-block's root as confirmed at these two slots while Nimbus and Lodestar still report the previous block's root — likely either a real client-level FCR difference or harness bias around the order of `recompute_head_at_slot` relative to attestation injection. Worth investigating before publishing as a real FCR divergence.

## How to reproduce

```bash
# Build everything once
go build -o results/fcr-orchestrator ./cmd/fcr-orchestrator
( cd engines/nimbus && bash build.sh )
( cd engines/teku && bash build.sh )
( cd engines/lodestar && bash build.sh )

# Single epoch smoke against the same BN (BEACON_NODE_URL in .env)
for engine in nimbus teku lodestar; do
  scripts/cross-client/run.sh "$engine" "./results/fcr-$engine"
done

# Per-slot diff
scripts/cross-client/diff.py
```

`results/cross-client/{engine}/epoch-N.{csv,jsonl,manifest.json}` is the per-engine raw output. `results/cross-client/{per-slot,divergences}.csv` is the cross-engine diff. The data directory is gitignored; commit only this snapshot if the divergence pattern is worth recording.
